package asynqmon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Integration tests for the live jobs event feed — the asynqmon:events:jobs
// pub/sub channel every Store mutation publishes on (jobs/events.go) and the
// `jobs` SSE event the fleet events broker relays it as — against the same
// real Redis DB 12 the jobs suite owns (jobs_handlers_test.go; only DB 12 is
// flushed here). Skipped when Redis does not answer PING.
//
// Convey discipline (same as jobs_handlers_test.go): flows that publish or
// stream run imperatively BEFORE the Convey tree; the tree only reads
// captured results.
//
// The `jobs` event body {"jobs":[<jobJSON>...]} and the pub/sub payload
// (= one jobJSON) are FROZEN frontend contracts (ui/src/hooks/useJobsEvents).
// ****************************************************************************

// subscribeJobsEvents opens a raw subscription on the jobs event channel and
// confirms it is live before returning (so no publish can be missed).
func subscribeJobsEvents(t *testing.T, rc *redis.Client) <-chan *redis.Message {
	t.Helper()
	sub := rc.Subscribe(context.Background(), jobs.EventsChannel)
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatalf("confirming jobs event subscription: %v", err)
	}
	t.Cleanup(func() { sub.Close() })
	return sub.Channel()
}

// nextJobEvent waits for one published job payload; ok=false on timeout.
func nextJobEvent(t *testing.T, ch <-chan *redis.Message, timeout time.Duration) (jobs.WireJob, bool) {
	t.Helper()
	select {
	case m := <-ch:
		var wj jobs.WireJob
		if err := json.Unmarshal([]byte(m.Payload), &wj); err != nil {
			t.Fatalf("decoding jobs event payload: %v\npayload: %s", err, m.Payload)
		}
		return wj, true
	case <-time.After(timeout):
		return jobs.WireJob{}, false
	}
}

// collectJobEvents drains every payload that arrives within the window.
func collectJobEvents(t *testing.T, ch <-chan *redis.Message, window time.Duration) []jobs.WireJob {
	t.Helper()
	var out []jobs.WireJob
	deadline := time.After(window)
	for {
		select {
		case m := <-ch:
			var wj jobs.WireJob
			if err := json.Unmarshal([]byte(m.Payload), &wj); err != nil {
				t.Fatalf("decoding jobs event payload: %v", err)
			}
			out = append(out, wj)
		case <-deadline:
			return out
		}
	}
}

// ----------------------------------------------------------------------------
// Pub/sub publishing: every state transition publishes immediately; progress-
// only batch writes coalesce to ≤1 publish per job per second.
// ----------------------------------------------------------------------------

func TestJobsEventPublishing(t *testing.T) {
	env := newJobsTestEnv(t)
	ctx := context.Background()
	events := subscribeJobsEvents(t, env.rc)

	// --- create: a job coming into existence publishes immediately ---------
	j := &jobs.Job{
		ID:        jobs.NewJobID(),
		Verb:      jobs.VerbArchive,
		Scope:     jobs.Scope{Queue: "evq", State: "pending"},
		Phase:     jobs.PhasePreview,
		State:     jobs.StatePreviewing,
		Throttle:  200,
		Reason:    "events test",
		Actor:     "tester",
		CostClass: jobs.CostCheap,
		CreatedAt: time.Now(),
	}
	if err := env.store.Create(ctx, j); err != nil {
		t.Fatalf("creating job: %v", err)
	}
	createdEvt, createdOK := nextJobEvent(t, events, 5*time.Second)

	// --- claim, then rapid progress-only writes: throttled to one ----------
	token, err := env.store.Claim(ctx, j.ID, "events-test", 15*time.Second)
	if err != nil || token == 0 {
		t.Fatalf("claiming job: token=%d err=%v", token, err)
	}
	time.Sleep(1100 * time.Millisecond) // clear the create-publish throttle window
	for i := 1; i <= 3; i++ {
		ok, werr := env.store.WriteProgress(ctx, j.ID, token, jobs.ProgressWrite{
			Fields: map[string]string{"candidates": string(rune('0' + i)), "scanned": string(rune('0' + i))},
		})
		if werr != nil || !ok {
			t.Fatalf("progress write %d: ok=%v err=%v", i, ok, werr)
		}
	}
	progressEvts := collectJobEvents(t, events, 500*time.Millisecond)

	// --- a state-carrying write publishes despite the fresh throttle stamp -
	if ok, werr := env.store.WriteProgress(ctx, j.ID, token, jobs.ProgressWrite{
		Fields: map[string]string{"state": string(jobs.StatePaused), "ctl": ""},
	}); werr != nil || !ok {
		t.Fatalf("pause write: ok=%v err=%v", ok, werr)
	}
	pausedEvt, pausedOK := nextJobEvent(t, events, 5*time.Second)

	// --- CompletePreview: transition, publishes immediately too ------------
	if ok, cerr := env.store.CompletePreview(ctx, j.ID, token); cerr != nil || !ok {
		t.Fatalf("complete preview: ok=%v err=%v", ok, cerr)
	}
	readyEvt, readyOK := nextJobEvent(t, events, 5*time.Second)

	// --- control-plane cancel of the parked job: terminal event ------------
	if _, cerr := env.store.RequestCancel(ctx, j.ID); cerr != nil {
		t.Fatalf("cancel: %v", cerr)
	}
	canceledEvt, canceledOK := nextJobEvent(t, events, 5*time.Second)

	Convey("Given a subscriber on the shared jobs event channel", t, func() {
		Convey("Then creating a job publishes its public JSON immediately", func() {
			So(createdOK, ShouldBeTrue)
			So(createdEvt.ID, ShouldEqual, j.ID)
			So(createdEvt.State, ShouldEqual, string(jobs.StatePreviewing))
			So(createdEvt.Verb, ShouldEqual, string(jobs.VerbArchive))
			So(createdEvt.Scope.Queue, ShouldEqual, "evq")
		})

		Convey("Then rapid progress-only writes coalesce to one publish per second", func() {
			So(progressEvts, ShouldHaveLength, 1)
			So(progressEvts[0].ID, ShouldEqual, j.ID)
			So(progressEvts[0].Counts.Candidates, ShouldEqual, 1) // the FIRST write published; later ones were suppressed
			So(progressEvts[0].Counts.Scanned, ShouldEqual, 1)
		})

		Convey("Then a state-carrying write bypasses the throttle", func() {
			So(pausedOK, ShouldBeTrue)
			So(pausedEvt.State, ShouldEqual, string(jobs.StatePaused))
			// The suppressed progress still rode along on the forced publish.
			So(pausedEvt.Counts.Candidates, ShouldEqual, 3)
		})

		Convey("Then CompletePreview publishes the preview_ready park", func() {
			So(readyOK, ShouldBeTrue)
			So(readyEvt.PreviewComplete, ShouldBeTrue)
			So(readyEvt.State, ShouldEqual, string(jobs.StatePreviewReady))
		})

		Convey("Then the control-plane cancel publishes the terminal state", func() {
			So(canceledOK, ShouldBeTrue)
			So(canceledEvt.State, ShouldEqual, string(jobs.StateCanceled))
			So(canceledEvt.FinishedAt, ShouldNotBeEmpty)
		})
	})
}

// ----------------------------------------------------------------------------
// SSE delivery: the broker relays the channel as `jobs` events, sends the
// active-jobs snapshot in the on-connect burst, and a real job driven by the
// runner streams through to its terminal state.
// ----------------------------------------------------------------------------

// jobsEventBody is the SSE `jobs` event body (frozen frontend contract).
type jobsEventBody struct {
	Jobs []jobs.WireJob `json:"jobs"`
}

func decodeJobsEvent(t *testing.T, fr sseFrame) jobsEventBody {
	t.Helper()
	var body jobsEventBody
	if err := json.Unmarshal([]byte(fr.data), &body); err != nil {
		t.Fatalf("decoding jobs SSE event: %v\ndata: %s", err, fr.data)
	}
	return body
}

// awaitJobsEventState reads `jobs` events until one carries the job in a
// state matching cond; every observed snapshot of the job is returned.
func awaitJobsEventState(t *testing.T, c *sseClient, jobID string, what string, cond func(jobs.WireJob) bool) []jobs.WireJob {
	t.Helper()
	var seen []jobs.WireJob
	for i := 0; i < 200; i++ {
		fr := c.nextEvent(t, "jobs")
		for _, wj := range decodeJobsEvent(t, fr).Jobs {
			if wj.ID != jobID {
				continue
			}
			seen = append(seen, wj)
			if cond(wj) {
				return seen
			}
		}
	}
	t.Fatalf("no jobs event satisfied %q for job %s (saw %d snapshots)", what, jobID, len(seen))
	return nil
}

func TestJobsSSEDelivery(t *testing.T) {
	env := newJobsTestEnv(t)
	ctx := context.Background()

	// A jobs-only broker: engine nil (stats disabled) — the stream must still
	// carry jobs events. Same production construction path as handler.go.
	broker := newFleetEventsBroker(nil, env.rc, env.store)
	broker.start(ctx)
	t.Cleanup(broker.stop)
	ts := httptest.NewServer(newFleetEventsHandlerFunc(broker))
	t.Cleanup(ts.Close)

	// Fixture: 5 pending tasks and a previewing job created BEFORE the SSE
	// client connects — it must arrive in the on-connect snapshot.
	seedPending(t, env, "sseq", "task:sse", `{"k":"v"}`, 5)
	j := &jobs.Job{
		ID:        jobs.NewJobID(),
		Verb:      jobs.VerbArchive,
		Scope:     jobs.Scope{Queue: "sseq", State: "pending"},
		Phase:     jobs.PhasePreview,
		State:     jobs.StatePreviewing,
		Throttle:  1000,
		Reason:    "sse delivery test",
		Actor:     "tester",
		CostClass: jobs.ComputeCostClass(jobs.Scope{Queue: "sseq", State: "pending"}, jobs.VerbArchive),
		CreatedAt: time.Now(),
	}
	if err := env.store.Create(ctx, j); err != nil {
		t.Fatalf("creating job: %v", err)
	}

	c := openSSE(t, ts.URL)
	defer c.close()
	hello := c.nextComment(t)
	snapshot := decodeJobsEvent(t, c.nextEvent(t, "jobs"))

	// The runner claims the job, enumerates the 5 candidates and parks it
	// preview_ready — every step must stream through as `jobs` events.
	env.startTestRunner(t)
	previewSeen := awaitJobsEventState(t, c, j.ID, "preview complete", func(wj jobs.WireJob) bool {
		return wj.PreviewComplete && wj.State == string(jobs.StatePreviewReady)
	})

	// Execute: the transition publishes, then the runner archives all 5 and
	// finalizes — the terminal event must arrive with the final counts.
	if _, err := env.store.RequestExecute(ctx, j.ID, false, 0, "sse test execute"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	execSeen := awaitJobsEventState(t, c, j.ID, "terminal done", func(wj jobs.WireJob) bool {
		return wj.State == string(jobs.StateDone)
	})
	terminal := execSeen[len(execSeen)-1]

	// A client connecting AFTER the job finished gets a snapshot WITHOUT the
	// terminal job (the snapshot is the active set; history stays on GET).
	c2 := openSSE(t, ts.URL)
	defer c2.close()
	c2.nextComment(t)
	postSnapshot := decodeJobsEvent(t, c2.nextEvent(t, "jobs"))

	Convey("Given a jobs-only SSE broker, a previewing job and a live runner", t, func() {
		Convey("Then the stream connects as an event stream even with stats disabled", func() {
			So(c.resp.StatusCode, ShouldEqual, http.StatusOK)
			So(c.resp.Header.Get("Content-Type"), ShouldStartWith, "text/event-stream")
			So(hello.comment, ShouldEqual, "connected")
		})

		Convey("Then the on-connect burst carries the active-jobs snapshot", func() {
			So(snapshot.Jobs, ShouldHaveLength, 1)
			So(snapshot.Jobs[0].ID, ShouldEqual, j.ID)
			So(snapshot.Jobs[0].State, ShouldEqual, string(jobs.StatePreviewing))
		})

		Convey("Then enumeration progress streams through to the preview_ready park", func() {
			last := previewSeen[len(previewSeen)-1]
			So(last.PreviewComplete, ShouldBeTrue)
			So(last.State, ShouldEqual, string(jobs.StatePreviewReady))
			So(last.Counts.Candidates, ShouldEqual, 5)
			So(last.Counts.Scanned, ShouldEqual, 5)
		})

		Convey("Then execution streams through to the terminal state with final counts", func() {
			So(terminal.State, ShouldEqual, string(jobs.StateDone))
			So(terminal.Phase, ShouldEqual, string(jobs.PhaseExecute))
			So(terminal.Counts.Acted, ShouldEqual, 5)
			So(terminal.Counts.Failed, ShouldEqual, 0)
			So(terminal.FinishedAt, ShouldNotBeEmpty)
		})

		Convey("Then a fresh connection's snapshot excludes terminal jobs", func() {
			So(postSnapshot.Jobs, ShouldBeEmpty)
		})
	})
}
