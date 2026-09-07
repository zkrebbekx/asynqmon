package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zkrebbekx/asynqmon/jobs"
	"github.com/zkrebbekx/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - GET /api/fleet/events — the SSE stream for live aggregates (§4.4)
//   - fleetEventsBroker: ONE goroutine per process that renders the fleet
//     payloads after each sweep (lease holder, via the engine's sweep
//     notifications) or each interval tick (standby replicas, reading the
//     shared cache) and fans the rendered bytes out to every subscriber.
//     N subscribers never multiply sweeps or cache reads.
//   - the jobs relay: ONE Redis pub/sub subscription per process on
//     asynqmon:events:jobs (jobs/events.go — the writing replica publishes
//     each job's public JSON on progress ticks and state transitions),
//     fanned out to the same subscribers as `jobs` events. Multi-replica
//     correct by construction: whichever replica runs the job publishes;
//     every replica's broker relays.
//
// Event contract (frozen with the frontend):
//   event: overview  — data = the GET /api/fleet/overview response JSON
//   event: attention — data = the GET /api/fleet/attention response JSON
//   event: jobs      — data = {"jobs": [<job public JSON>, ...]}; the
//                      on-connect burst carries the current NON-TERMINAL jobs
//                      snapshot (served from the broker cache the relay keeps
//                      current), every later event carries the one job that
//                      just progressed / transitioned
//   event: ping      — data = {"t": <unix ms>}; the liveness beat the browser
//                      watchdog listens for (EventSource never surfaces a
//                      comment)
// plus a comment heartbeat every 15s so proxies keep the connection open.
// ****************************************************************************

const (
	// fleetEventsHeartbeat is how often a stream writes its comment line and
	// its `ping` event.
	fleetEventsHeartbeat = 15 * time.Second

	// defaultMaxSSEConnections caps concurrent subscribers per replica when
	// Options.MaxSSEConnections is 0. Each connection costs a goroutine, a
	// 16-slot channel and a ticker.
	defaultMaxSSEConnections = 256

	// sseRetryAfterSeconds is the Retry-After value on the 503 a refused
	// subscriber gets.
	sseRetryAfterSeconds = "5"

	// sseWriteTimeout bounds one write to one subscriber. Without it a
	// half-open TCP peer blocks the handler goroutine in Write until the
	// kernel keepalive gives up (2 minutes or more).
	sseWriteTimeout = 30 * time.Second

	// jobsSnapshotReconcile is how long the broker serves its cached
	// on-connect jobs snapshot before it reads Redis again. The relay keeps
	// the cache exact between reconciles by applying every jobs event to it,
	// so the read only repairs losses (a dropped pub/sub message, a job
	// whose hash expired). A reconnect storm — every browser tab
	// reconnecting after a pod restart — therefore costs one read, not one
	// per connection.
	jobsSnapshotReconcile = 30 * time.Second
)

// fleetEventsPayload is one broadcast unit: pre-rendered JSON per event. A
// nil slice means that payload was unavailable (engine warming up, or the
// broadcast is a jobs-only tick) and its event is skipped rather than
// fabricated.
type fleetEventsPayload struct {
	overview  []byte
	attention []byte
	// jobs is one {"jobs":[...]} body (relay tick or on-connect snapshot).
	jobs []byte
}

// fleetEventsBroker owns the render-and-fan-out loop. Subscriber channels
// are buffered and written non-blockingly: every payload is a full snapshot,
// so a slow client that misses one simply renders the next — no backpressure
// ever reaches the sweeper. (Jobs ticks are incremental, but they are
// per-job snapshots re-published at least once per state transition, and the
// frontend keeps a poll fallback — a dropped tick self-heals.)
type fleetEventsBroker struct {
	// engine may be nil (stats disabled): the overview/attention loop then
	// never runs and the stream carries jobs events + heartbeats only.
	engine *stats.Engine
	// rc/jobsStore feed the `jobs` event (relay + on-connect snapshot); nil
	// disables it (direct-router tests without jobs wiring).
	rc        redis.UniversalClient
	jobsStore *jobs.Store
	// heartbeatEvery is per-connection; a field (not the const) so tests can
	// shrink it.
	heartbeatEvery time.Duration
	// maxSubs caps concurrent subscribers; <= 0 means unlimited.
	maxSubs int

	// jobsSnapMu guards the cached on-connect jobs snapshot. It is separate
	// from mu so a snapshot read never blocks a fan-out. jobsActive is the
	// current non-terminal job set by id, jobsSnap its rendered
	// {"jobs":[...]} body, and jobsSyncedAt the last full Redis reconcile.
	jobsSnapMu   sync.Mutex
	jobsActive   map[string]jobs.WireJob
	jobsSnap     []byte
	jobsSyncedAt time.Time

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

// newFleetEventsBroker creates the broker. maxSubs caps concurrent
// subscribers: 0 uses defaultMaxSSEConnections, a negative value removes the
// cap.
func newFleetEventsBroker(engine *stats.Engine, rc redis.UniversalClient, jobsStore *jobs.Store, maxSubs int) *fleetEventsBroker {
	if maxSubs == 0 {
		maxSubs = defaultMaxSSEConnections
	}
	return &fleetEventsBroker{
		engine:         engine,
		rc:             rc,
		jobsStore:      jobsStore,
		heartbeatEvery: fleetEventsHeartbeat,
		maxSubs:        maxSubs,
		subs:           make(map[chan fleetEventsPayload]struct{}),
	}
}

// start launches the broker loops. Safe to call once per stop.
func (b *fleetEventsBroker) start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return
	}
	b.started = true
	ctx, b.cancel = context.WithCancel(ctx)
	if b.engine != nil {
		b.wg.Add(1)
		go b.run(ctx)
	}
	if b.rc != nil {
		b.startJobsRelay(ctx)
	}
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

// startJobsRelay subscribes this process ONCE to the shared jobs event
// channel and fans every message out as a `jobs` SSE event. Shutdown is
// deterministic: stop() cancels the context, the closer goroutine closes the
// pubsub, its Channel() closes, and the relay loop exits (both goroutines are
// wg-tracked, so stop() waits for them — no leaks, -race clean).
func (b *fleetEventsBroker) startJobsRelay(ctx context.Context) {
	pubsub := b.rc.Subscribe(ctx, jobs.EventsChannel)
	b.wg.Add(2)
	go func() {
		defer b.wg.Done()
		<-ctx.Done()
		_ = pubsub.Close()
	}()
	go func() {
		defer b.wg.Done()
		for msg := range pubsub.Channel() {
			if b.subscriberCount() == 0 {
				continue
			}
			// The payload is one job's public JSON object (jobs/events.go);
			// wrap it in the uniform {"jobs":[...]} event body.
			b.fanOutJobs([]byte(`{"jobs":[` + msg.Payload + `]}`))
			// Keep the on-connect snapshot exact while jobs move, without
			// re-reading Redis: the event IS the job's new state.
			b.applyJobsEvent([]byte(msg.Payload))
		}
	}()
}

// fanOutJobs delivers one jobs event body to every subscriber,
// non-blockingly (same no-backpressure rule as publish).
func (b *fleetEventsBroker) fanOutJobs(body []byte) {
	p := fleetEventsPayload{jobs: body}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- p:
		default: // slow client: the poll fallback / next tick self-heals
		}
	}
}

// applyJobsEvent folds one relayed job event into the cached snapshot: a
// non-terminal job replaces its entry, a terminal job leaves the active set.
// Callers hold no lock.
func (b *fleetEventsBroker) applyJobsEvent(payload []byte) {
	var wj jobs.WireJob
	if json.Unmarshal(payload, &wj) != nil || wj.ID == "" {
		return
	}
	b.jobsSnapMu.Lock()
	defer b.jobsSnapMu.Unlock()
	if b.jobsActive == nil {
		// No snapshot has been built yet: the next connect reconciles from
		// Redis and picks this job up there.
		return
	}
	if jobs.State(wj.State).IsTerminal() {
		delete(b.jobsActive, wj.ID)
	} else {
		b.jobsActive[wj.ID] = wj
	}
	b.renderCachedJobsLocked()
}

// renderCachedJobsLocked re-renders jobsSnap from jobsActive, newest first.
// The caller holds jobsSnapMu.
func (b *fleetEventsBroker) renderCachedJobsLocked() {
	active := make([]jobs.WireJob, 0, len(b.jobsActive))
	for _, wj := range b.jobsActive {
		active = append(active, wj)
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].CreatedAt != active[j].CreatedAt {
			return active[i].CreatedAt > active[j].CreatedAt // newest first
		}
		return active[i].ID < active[j].ID
	})
	if body, err := json.Marshal(map[string]interface{}{"jobs": active}); err == nil {
		b.jobsSnap = body
	}
}

// cachedJobsSnapshot serves the on-connect snapshot from the broker cache.
// It reads Redis only when the cache is empty or older than
// jobsSnapshotReconcile, so N connections cost at most one read per
// reconcile window instead of one read each. Returns nil when the broker has
// no jobs wiring or the first read fails (the event is then skipped, never
// fabricated).
func (b *fleetEventsBroker) cachedJobsSnapshot(ctx context.Context) []byte {
	b.jobsSnapMu.Lock()
	defer b.jobsSnapMu.Unlock()
	if b.jobsSnap != nil && time.Since(b.jobsSyncedAt) < jobsSnapshotReconcile {
		return b.jobsSnap
	}
	active, ok := b.readActiveJobs(ctx)
	if !ok {
		return b.jobsSnap // a failed read keeps the previous copy, never fabricates
	}
	b.jobsActive = active
	b.jobsSyncedAt = time.Now()
	b.renderCachedJobsLocked()
	return b.jobsSnap
}

// readActiveJobs loads the current non-terminal jobs from the store. ok is
// false when the broker has no jobs wiring or the read failed.
func (b *fleetEventsBroker) readActiveJobs(ctx context.Context) (map[string]jobs.WireJob, bool) {
	if b.jobsStore == nil {
		return nil, false
	}
	list, err := b.jobsStore.List(ctx, 200)
	if err != nil {
		return nil, false
	}
	active := make(map[string]jobs.WireJob, len(list))
	for _, j := range list {
		if !j.State.IsTerminal() {
			active[j.ID] = jobs.WireOf(j)
		}
	}
	return active, true
}

// subscribe registers a new SSE connection. Returns the event channel, the
// last broadcast payload (nil when the broker has not rendered yet — the
// handler then renders once itself for the on-connect send), a cancel func
// the handler MUST call on disconnect, and ok=false when the replica already
// serves maxSubs streams (the handler then answers 503).
func (b *fleetEventsBroker) subscribe() (<-chan fleetEventsPayload, *fleetEventsPayload, func(), bool) {
	// Buffer sized for a burst of jobs ticks on top of the aggregate pair; a
	// full buffer only ever costs one tick, never blocks a publisher.
	ch := make(chan fleetEventsPayload, 16)
	b.mu.Lock()
	if b.maxSubs > 0 && len(b.subs) >= b.maxSubs {
		b.mu.Unlock()
		return nil, nil, nil, false
	}
	b.subs[ch] = struct{}{}
	last := b.last
	b.mu.Unlock()
	return ch, last, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}, true
}

// render builds both payloads from the engine's current state (in-process on
// the holder, shared cache on standbys) plus their change stamps. Payloads
// that are not ready yet come back nil.
func (b *fleetEventsBroker) render(ctx context.Context) (p fleetEventsPayload, overviewStamp time.Time, attentionStamp string) {
	if b.engine == nil {
		return p, overviewStamp, attentionStamp
	}
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

// writeFleetEvents writes the SSE events present in the payload. json.Marshal
// output contains no raw newlines, so a single data: line per event is
// well-formed.
func writeFleetEvents(w io.Writer, p fleetEventsPayload) {
	if p.overview != nil {
		fmt.Fprintf(w, "event: overview\ndata: %s\n\n", p.overview)
	}
	if p.attention != nil {
		fmt.Fprintf(w, "event: attention\ndata: %s\n\n", p.attention)
	}
	if p.jobs != nil {
		fmt.Fprintf(w, "event: jobs\ndata: %s\n\n", p.jobs)
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

		// Take a slot BEFORE writing any header: a refused subscriber must
		// get a plain 503, not a truncated event stream.
		ch, last, unsubscribe, ok := broker.subscribe()
		if !ok {
			w.Header().Set("Retry-After", sseRetryAfterSeconds)
			writeErrorMsg(w, http.StatusServiceUnavailable,
				"too many live event streams on this replica — retry in a few seconds (--max-sse-connections)")
			return
		}
		defer unsubscribe()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // defeat nginx response buffering
		w.WriteHeader(http.StatusOK)

		// flushWithDeadline writes whatever body was produced, bounded by
		// sseWriteTimeout, and clears the deadline again afterwards: an
		// idle stream must outlive the server's WriteTimeout, but a stuck
		// peer must be dropped in 30s instead of blocking this goroutine
		// until the kernel keepalive gives up. Returns false when the
		// stream is dead.
		flushWithDeadline := func(write func()) bool {
			_ = ctrl.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			write()
			err := ctrl.Flush()
			_ = ctrl.SetWriteDeadline(time.Time{})
			return err == nil
		}

		// On-connect: a comment so the client sees bytes immediately, then
		// the current state (broker's last broadcast, or a fresh render when
		// this is the first subscriber since boot), then the current
		// non-terminal jobs snapshot so job trackers re-attach without a
		// poll round-trip. The snapshot comes from the broker cache, so a
		// reconnect storm costs one Redis read, not one per connection.
		if last == nil {
			p, _, _ := broker.render(r.Context())
			last = &p
		}
		snap := broker.cachedJobsSnapshot(r.Context())
		if !flushWithDeadline(func() {
			io.WriteString(w, ": connected\n\n")
			writeFleetEvents(w, *last)
			if snap != nil {
				writeFleetEvents(w, fleetEventsPayload{jobs: snap})
			}
		}) {
			return
		}

		heartbeat := time.NewTicker(broker.heartbeatEvery)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return // client disconnected (or server shutting down)
			case p := <-ch:
				if !flushWithDeadline(func() { writeFleetEvents(w, p) }) {
					return
				}
			case <-heartbeat.C:
				// The comment keeps proxies from idling the connection out;
				// the `ping` event is the one EventSource surfaces, so the
				// browser can run a liveness watchdog (#51).
				if !flushWithDeadline(func() {
					io.WriteString(w, ": heartbeat\n\n")
					writePingEvent(w, time.Now())
				}) {
					return
				}
			}
		}
	}
}

// writePingEvent writes the liveness event the frontend watchdog listens for
// (#51): `event: ping` with the server time in unix milliseconds.
func writePingEvent(w io.Writer, now time.Time) {
	fmt.Fprintf(w, "event: ping\ndata: {\"t\":%d}\n\n", now.UnixMilli())
}
