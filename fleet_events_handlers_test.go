package asynqmon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Handler tests for GET /api/fleet/attention and GET /api/fleet/events,
// against the same real Redis DB 13 the other root handler tests use (see
// stats_handlers_test.go). Skipped when Redis does not answer PING.
//
// The attention/SSE payload shapes asserted here are FROZEN frontend
// contracts — if one of these tests forces a change, the frontend engineer
// must sign off first.
// ****************************************************************************

func TestFleetAttentionHandler(t *testing.T) {
	engine := newStatsTestEngine(t)
	// Second sweep: NO_CONSUMERS debounces over 2 consecutive sweeps.
	if err := engine.SweepNow(context.Background()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	handler := newFleetAttentionHandlerFunc(engine)

	Convey("Given a twice-swept fleet with pending work and no servers", t, func() {
		Convey("When GET /api/fleet/attention is served", func() {
			var rep stats.AttentionReport
			w := getJSON(t, handler, "/api/fleet/attention", &rep)

			Convey("Then it returns the ranked NO_CONSUMERS findings", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(rep.Findings, ShouldHaveLength, 3) // alpha, beta, gamma all uncovered
				// Ranked by severity then value: gamma (5 pending) first.
				So(rep.Findings[0].Queue, ShouldEqual, "gamma")
				So(rep.Findings[0].Detector, ShouldEqual, stats.DetectorNoConsumers)
				So(rep.Findings[0].Severity, ShouldEqual, 5)
				So(rep.Findings[0].Value, ShouldEqual, 5)
				So(rep.Findings[0].SuggestedQuery, ShouldEqual, "state=pending queue=gamma")
				So(rep.Findings[1].Queue, ShouldEqual, "beta")
				So(rep.Findings[2].Queue, ShouldEqual, "alpha")
			})
			Convey("Then the report footer states the detector census", func() {
				So(rep.DetectorsLive, ShouldEqual, 8)
				So(rep.DetectorsLearning, ShouldEqual, 0)
				So(rep.UpdatedAt, ShouldNotBeEmpty)
			})
		})

		Convey("When the raw body is inspected for the frozen contract keys", func() {
			w := getJSON(t, handler, "/api/fleet/attention", nil)
			var body map[string]interface{}
			So(json.Unmarshal(w.Body.Bytes(), &body), ShouldBeNil)

			Convey("Then the top-level and finding field names match the contract exactly", func() {
				for _, key := range []string{"findings", "updated_at", "detectors_live", "detectors_learning"} {
					_, ok := body[key]
					So(ok, ShouldBeTrue)
				}
				findings := body["findings"].([]interface{})
				So(findings, ShouldNotBeEmpty)
				finding := findings[0].(map[string]interface{})
				for _, key := range []string{"queue", "detector", "severity", "sentence", "value", "chips", "since", "suggested_query"} {
					_, ok := finding[key]
					So(ok, ShouldBeTrue)
				}
			})
		})

		Convey("When the stats engine is disabled (nil)", func() {
			w := getJSON(t, newFleetAttentionHandlerFunc(nil), "/api/fleet/attention", nil)
			Convey("Then it responds 503 with a JSON error", func() {
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(w.Body.String(), ShouldContainSubstring, "disabled")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// SSE plumbing helpers.
// ----------------------------------------------------------------------------

// sseFrame is one parsed server-sent block (up to the blank-line separator).
type sseFrame struct {
	event   string
	data    string
	comment string
}

type sseClient struct {
	resp   *http.Response
	br     *bufio.Reader
	cancel context.CancelFunc
}

// openSSE connects to the events endpoint with a bounded context so a broken
// stream fails the test instead of hanging it.
func openSSE(t *testing.T, url string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("building SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connecting SSE: %v", err)
	}
	return &sseClient{resp: resp, br: bufio.NewReader(resp.Body), cancel: cancel}
}

func (c *sseClient) close() {
	c.cancel()
	c.resp.Body.Close()
}

// nextFrame reads one frame; the request context bounds the wait.
func (c *sseClient) nextFrame(t *testing.T) sseFrame {
	t.Helper()
	var fr sseFrame
	seen := false
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v (frame so far: %+v)", err, fr)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if seen {
				return fr
			}
		case strings.HasPrefix(line, ":"):
			fr.comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			seen = true
		case strings.HasPrefix(line, "event:"):
			fr.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			seen = true
		case strings.HasPrefix(line, "data:"):
			fr.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			seen = true
		}
	}
}

// nextEvent skips comment frames (heartbeats) until a named event arrives.
func (c *sseClient) nextEvent(t *testing.T, name string) sseFrame {
	t.Helper()
	for {
		fr := c.nextFrame(t)
		if fr.event == name {
			return fr
		}
		if fr.event != "" {
			t.Fatalf("expected event %q, got %q (data: %s)", name, fr.event, fr.data)
		}
		// comment frame: keep reading
	}
}

// nextComment skips nothing: it demands the next frame be a comment.
func (c *sseClient) nextComment(t *testing.T) sseFrame {
	t.Helper()
	fr := c.nextFrame(t)
	if fr.comment == "" {
		t.Fatalf("expected a comment frame, got %+v", fr)
	}
	return fr
}

// noFlushWriter hides http.Flusher (and offers no Unwrap), simulating a
// middleware stack that cannot stream.
type noFlushWriter struct {
	http.ResponseWriter
}

func eventuallyTrue(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestFleetEventsHandler(t *testing.T) {
	engine := newStatsTestEngine(t)
	broker := newFleetEventsBroker(engine)
	broker.heartbeatEvery = 40 * time.Millisecond // testable heartbeat cadence
	broker.start(context.Background())
	t.Cleanup(broker.stop)

	ts := httptest.NewServer(newFleetEventsHandlerFunc(broker))
	t.Cleanup(ts.Close)

	// --- interact with the stream imperatively, assert afterwards ---------

	// Two concurrent subscribers, both fed by the single broker.
	c1 := openSSE(t, ts.URL)
	defer c1.close()
	c2 := openSSE(t, ts.URL)
	defer c2.close()

	// On connect: current state immediately (after the connected comment).
	c1Hello := c1.nextComment(t)
	c1InitOverview := c1.nextEvent(t, "overview")
	c1InitAttention := c1.nextEvent(t, "attention")
	c2.nextComment(t)
	c2.nextEvent(t, "overview")
	c2.nextEvent(t, "attention")

	// A sweep completion must push a fresh overview + attention pair to
	// every subscriber (fan-out from the one broker — the sweep ran once).
	if err := engine.SweepNow(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	c1Overview := c1.nextEvent(t, "overview")
	c1Attention := c1.nextEvent(t, "attention")
	c2Overview := c2.nextEvent(t, "overview")
	c2Attention := c2.nextEvent(t, "attention")

	// With no further sweeps, the next traffic is the comment heartbeat.
	c1Heartbeat := c1.nextComment(t)

	// Disconnecting must unsubscribe (no goroutine or registration leaks).
	c2.close()
	c2Cleaned := eventuallyTrue(5*time.Second, func() bool { return broker.subscriberCount() == 1 })

	// The GET endpoint the SSE payload must mirror.
	var restOverview fleetOverviewResponse
	getJSON(t, newFleetOverviewHandlerFunc(engine), "/api/fleet/overview", &restOverview)

	Convey("Given a swept fleet, a running broker and two SSE subscribers", t, func() {
		Convey("Then the stream declares itself as an event stream", func() {
			So(c1.resp.StatusCode, ShouldEqual, http.StatusOK)
			So(c1.resp.Header.Get("Content-Type"), ShouldStartWith, "text/event-stream")
			So(c1Hello.comment, ShouldEqual, "connected")
		})

		Convey("Then the on-connect events carry the current state", func() {
			So(c1InitOverview.data, ShouldNotBeEmpty)
			So(c1InitAttention.data, ShouldNotBeEmpty)
			var rep stats.AttentionReport
			So(json.Unmarshal([]byte(c1InitAttention.data), &rep), ShouldBeNil)
			So(rep.DetectorsLive, ShouldEqual, 8)
			// One sweep so far: NO_CONSUMERS is still debouncing.
			So(rep.Findings, ShouldBeEmpty)
		})

		Convey("Then the overview event payload IS the /api/fleet/overview JSON", func() {
			var ov fleetOverviewResponse
			So(json.Unmarshal([]byte(c1Overview.data), &ov), ShouldBeNil)
			So(ov.Fleet.QueuesTotal, ShouldEqual, restOverview.Fleet.QueuesTotal)
			So(ov.Fleet.Pending, ShouldEqual, restOverview.Fleet.Pending)
			So(ov.Fleet.RedisMemoryUsedBytes, ShouldBeGreaterThan, 0)
			So(ov.Coverage.UpdatedAt, ShouldEqual, restOverview.Coverage.UpdatedAt)
		})

		Convey("Then the post-sweep attention event reflects the debounced findings", func() {
			var rep stats.AttentionReport
			So(json.Unmarshal([]byte(c1Attention.data), &rep), ShouldBeNil)
			So(rep.Findings, ShouldHaveLength, 3) // NO_CONSUMERS x alpha/beta/gamma
			So(rep.Findings[0].Detector, ShouldEqual, stats.DetectorNoConsumers)
		})

		Convey("Then both subscribers received the same sweep fan-out", func() {
			So(c2Overview.data, ShouldEqual, c1Overview.data)
			So(c2Attention.data, ShouldEqual, c1Attention.data)
		})

		Convey("Then an idle stream heartbeats with a comment", func() {
			So(c1Heartbeat.comment, ShouldEqual, "heartbeat")
		})

		Convey("Then a disconnected subscriber is unregistered", func() {
			So(c2Cleaned, ShouldBeTrue)
		})
	})
}

func TestFleetEventsHandlerUnavailable(t *testing.T) {
	engine := newStatsTestEngine(t)
	broker := newFleetEventsBroker(engine)
	broker.start(context.Background())
	t.Cleanup(broker.stop)

	Convey("Given the SSE handler", t, func() {
		Convey("When the stats engine is disabled (nil broker)", func() {
			w := httptest.NewRecorder()
			newFleetEventsHandlerFunc(nil)(w, httptest.NewRequest("GET", "/api/fleet/events", nil))
			Convey("Then it responds 503 with a JSON error", func() {
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(w.Body.String(), ShouldContainSubstring, "disabled")
			})
		})

		Convey("When the ResponseWriter cannot flush", func() {
			rec := httptest.NewRecorder()
			w := &noFlushWriter{ResponseWriter: rec}
			newFleetEventsHandlerFunc(broker)(w, httptest.NewRequest("GET", "/api/fleet/events", nil))
			Convey("Then it responds 503 JSON instead of a dead stream", func() {
				So(rec.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(rec.Body.String(), ShouldContainSubstring, "streaming unsupported")
				So(rec.Header().Get("Content-Type"), ShouldStartWith, "application/json")
			})
		})
	})
}
