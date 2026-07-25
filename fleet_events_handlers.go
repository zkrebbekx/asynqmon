package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - GET /api/fleet/events — the SSE stream for live aggregates (§4.4)
//   - fleetEventsBroker: ONE goroutine per process that renders the fleet
//     payloads after each sweep (lease holder, via the engine's sweep
//     notifications) or each interval tick (standby replicas, reading the
//     shared cache) and fans the rendered bytes out to every subscriber.
//     N subscribers never multiply sweeps or cache reads.
//
// Event contract (frozen with the frontend):
//   event: overview  — data = the GET /api/fleet/overview response JSON
//   event: attention — data = the GET /api/fleet/attention response JSON
// plus a comment heartbeat every 15s so proxies keep the connection open.
// ****************************************************************************

// fleetEventsHeartbeat is how often an idle stream writes a comment line.
const fleetEventsHeartbeat = 15 * time.Second

// fleetEventsPayload is one broadcast unit: pre-rendered JSON for both
// events. A nil slice means that payload was unavailable (engine warming up)
// and its event is skipped rather than fabricated.
type fleetEventsPayload struct {
	overview  []byte
	attention []byte
}

// fleetEventsBroker owns the render-and-fan-out loop. Subscriber channels
// are buffered and written non-blockingly: every payload is a full snapshot,
// so a slow client that misses one simply renders the next — no backpressure
// ever reaches the sweeper.
type fleetEventsBroker struct {
	engine *stats.Engine
	// heartbeatEvery is per-connection; a field (not the const) so tests can
	// shrink it.
	heartbeatEvery time.Duration

	mu   sync.Mutex
	subs map[chan fleetEventsPayload]struct{}
	// last is the most recently broadcast payload, handed to new subscribers
	// for their immediate on-connect send.
	last *fleetEventsPayload
	// Change stamps of the last broadcast, so the holder's sweep
	// notification followed by the interval tick does not double-send an
	// identical snapshot.
	lastOverviewStamp  time.Time
	lastAttentionStamp string

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

func newFleetEventsBroker(engine *stats.Engine) *fleetEventsBroker {
	return &fleetEventsBroker{
		engine:         engine,
		heartbeatEvery: fleetEventsHeartbeat,
		subs:           make(map[chan fleetEventsPayload]struct{}),
	}
}

// start launches the broker loop. Safe to call once per stop.
func (b *fleetEventsBroker) start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return
	}
	b.started = true
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.run(ctx)
}

// stop cancels the loop and waits for it to exit (no goroutine leaks).
// Connected clients are not force-closed here; their handlers exit with the
// request context (server shutdown closes those).
func (b *fleetEventsBroker) stop() {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	b.started = false
	cancel := b.cancel
	b.mu.Unlock()
	cancel()
	b.wg.Wait()
}

// run is the single notifier: woken by local sweep completions (lease
// holder) and by the interval ticker (standbys following the shared cache;
// also the holder's fallback if sweeps stall). It renders at most once per
// wake and only when someone is listening.
func (b *fleetEventsBroker) run(ctx context.Context) {
	defer b.wg.Done()
	sweeps, cancelSub := b.engine.SubscribeSweeps()
	defer cancelSub()
	ticker := time.NewTicker(b.engine.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweeps:
		case <-ticker.C:
		}
		if b.subscriberCount() == 0 {
			continue // nobody listening: skip the render (and its cache read)
		}
		b.publish(ctx)
	}
}

func (b *fleetEventsBroker) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// subscribe registers a new SSE connection. Returns the event channel, the
// last broadcast payload (nil when the broker has not rendered yet — the
// handler then renders once itself for the on-connect send), and a cancel
// func the handler MUST call on disconnect.
func (b *fleetEventsBroker) subscribe() (<-chan fleetEventsPayload, *fleetEventsPayload, func()) {
	ch := make(chan fleetEventsPayload, 4)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	last := b.last
	b.mu.Unlock()
	return ch, last, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// render builds both payloads from the engine's current state (in-process on
// the holder, shared cache on standbys) plus their change stamps. Payloads
// that are not ready yet come back nil.
func (b *fleetEventsBroker) render(ctx context.Context) (p fleetEventsPayload, overviewStamp time.Time, attentionStamp string) {
	now := time.Now()
	if res, err := b.engine.Read(ctx); err == nil {
		if data, err := json.Marshal(buildFleetOverviewResponse(res, now)); err == nil {
			p.overview = data
			overviewStamp = res.Fleet.RefreshedAt
		}
	}
	if rep, err := b.engine.ReadAttention(ctx); err == nil {
		if data, err := json.Marshal(rep); err == nil {
			p.attention = data
			attentionStamp = rep.UpdatedAt
		}
	}
	return p, overviewStamp, attentionStamp
}

// publish renders once and fans out to all subscribers, deduplicating
// unchanged snapshots (sweep notification + tick landing in the same
// interval).
func (b *fleetEventsBroker) publish(ctx context.Context) {
	p, ovStamp, atStamp := b.render(ctx)
	if p.overview == nil && p.attention == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ovStamp.Equal(b.lastOverviewStamp) && atStamp == b.lastAttentionStamp {
		return
	}
	b.lastOverviewStamp, b.lastAttentionStamp = ovStamp, atStamp
	b.last = &p
	for ch := range b.subs {
		select {
		case ch <- p:
		default: // slow client: it will catch the next full snapshot
		}
	}
}

// writeFleetEvents writes the two SSE events. json.Marshal output contains
// no raw newlines, so a single data: line per event is well-formed.
func writeFleetEvents(w io.Writer, p fleetEventsPayload) {
	if p.overview != nil {
		fmt.Fprintf(w, "event: overview\ndata: %s\n\n", p.overview)
	}
	if p.attention != nil {
		fmt.Fprintf(w, "event: attention\ndata: %s\n\n", p.attention)
	}
}

// canFlushResponse reports whether the ResponseWriter (or anything it wraps,
// via the http.ResponseController Unwrap convention) supports flushing —
// without which SSE would buffer forever.
func canFlushResponse(w http.ResponseWriter) bool {
	for {
		switch v := w.(type) {
		case http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return false
		}
	}
}

// newFleetEventsHandlerFunc serves GET /api/fleet/events: text/event-stream,
// current state immediately on connect, then an overview + attention event
// pair after every sweep/tick the broker observes, with comment heartbeats
// in between. One subscription per connection; the broker does the work.
func newFleetEventsHandlerFunc(broker *fleetEventsBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if broker == nil {
			writeErrorMsg(w, http.StatusServiceUnavailable, "stats engine is disabled")
			return
		}
		if !canFlushResponse(w) {
			writeErrorMsg(w, http.StatusServiceUnavailable,
				"streaming unsupported: the response writer cannot flush (server middleware must pass http.Flusher through)")
			return
		}
		ctrl := http.NewResponseController(w)
		// An SSE stream must outlive the server's WriteTimeout; clear the
		// write deadline for this response only. Best-effort: on servers
		// without deadline support the stream simply lives within whatever
		// timeout applies.
		_ = ctrl.SetWriteDeadline(time.Time{})

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // defeat nginx response buffering
		w.WriteHeader(http.StatusOK)

		ch, last, unsubscribe := broker.subscribe()
		defer unsubscribe()

		// On-connect: a comment so the client sees bytes immediately, then
		// the current state (broker's last broadcast, or a fresh render when
		// this is the first subscriber since boot).
		io.WriteString(w, ": connected\n\n")
		if last == nil {
			p, _, _ := broker.render(r.Context())
			last = &p
		}
		writeFleetEvents(w, *last)
		if err := ctrl.Flush(); err != nil {
			return
		}

		heartbeat := time.NewTicker(broker.heartbeatEvery)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return // client disconnected (or server shutting down)
			case p := <-ch:
				writeFleetEvents(w, p)
				if err := ctrl.Flush(); err != nil {
					return
				}
			case <-heartbeat.C:
				io.WriteString(w, ": heartbeat\n\n")
				if err := ctrl.Flush(); err != nil {
					return
				}
			}
		}
	}
}
