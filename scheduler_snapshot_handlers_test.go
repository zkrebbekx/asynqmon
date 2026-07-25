package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Integration tests for the Schedulers screen endpoints (§3.8/§5.12) against
// a real Redis on 127.0.0.1:6379 DB 10. (DB ownership: 11 = coverage, 12 =
// jobs, 13 = fleet handlers, 14 = stats package; DB 10 belongs to the
// Schedulers tests — only DB 10 is flushed here.) Skipped when Redis does
// not answer PING.
//
// Fixtures are authentic end to end: a real asynq.Scheduler registers the
// entries (heartbeats write them; enqueue events are recorded by asynq), a
// real asynq.Server completes/fails the produced tasks so the outcome joins
// read genuine task states, and the stats engine's sweep persists the
// snapshots. Nothing is hand-built. The scheduler's first heartbeat lands at
// ~5s (asynq's fixed cadence), so this suite waits it out.
// ****************************************************************************

const (
	schedTestRedisAddr = "127.0.0.1:6379"
	schedTestRedisDB   = 10
)

// schedFixture is the shared end-to-end environment.
type schedFixture struct {
	rc        redis.UniversalClient
	insp      *asynq.Inspector
	scheduler *asynq.Scheduler
	server    *asynq.Server
	engine    *stats.Engine
	goneAfter time.Duration
}

// schedWaitFor polls cond until true or fatal timeout.
func schedWaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// newSchedFixture flushes DB 10, starts a scheduler with three entries —
//
//	sched:ok     @every 1s  Queue("sq")  Retention(1h)  → completes, retained
//	sched:fail   @every 1s  Queue("sq")  MaxRetry(3)    → fails, parked in retry
//	sched:noret  @every 1s  default queue, Retention 0  → completes, deleted
//
// — plus a server working both queues, waits until every outcome kind has
// materialized, then runs one stats sweep.
func newSchedFixture(t *testing.T) *schedFixture {
	t.Helper()
	ctx := context.Background()

	rc := redis.NewClient(&redis.Options{Addr: schedTestRedisAddr, DB: schedTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", schedTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })

	opt := asynq.RedisClientOpt{Addr: schedTestRedisAddr, DB: schedTestRedisDB}
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { insp.Close() })

	srv := asynq.NewServer(opt, asynq.Config{
		Concurrency: 4,
		Queues:      map[string]int{"sq": 1, "default": 1},
		LogLevel:    asynq.FatalLevel,
		// Park failures in retry state for the whole test.
		RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return time.Hour },
	})
	muxsrv := asynq.NewServeMux()
	muxsrv.HandleFunc("sched:ok", func(context.Context, *asynq.Task) error { return nil })
	muxsrv.HandleFunc("sched:fail", func(context.Context, *asynq.Task) error { return errors.New("boom") })
	muxsrv.HandleFunc("sched:noret", func(context.Context, *asynq.Task) error { return nil })
	if err := srv.Start(muxsrv); err != nil {
		t.Fatalf("starting asynq server: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	scheduler := asynq.NewScheduler(opt, &asynq.SchedulerOpts{LogLevel: asynq.FatalLevel})
	register := func(spec, typ string, opts ...asynq.Option) {
		t.Helper()
		if _, err := scheduler.Register(spec, asynq.NewTask(typ, []byte(`{"fixture":true}`)), opts...); err != nil {
			t.Fatalf("registering %s: %v", typ, err)
		}
	}
	register("@every 1s", "sched:ok", asynq.Queue("sq"), asynq.Retention(time.Hour))
	register("@every 1s", "sched:fail", asynq.Queue("sq"), asynq.MaxRetry(3))
	register("@every 1s", "sched:noret")
	if err := scheduler.Start(); err != nil {
		t.Fatalf("starting scheduler: %v", err)
	}
	// Shutdown is idempotent; tests that exercise the GONE transition call it
	// early and this cleanup becomes a no-op.
	t.Cleanup(scheduler.Shutdown)

	// The scheduler's first heartbeat writes the entries after ~5s.
	schedWaitFor(t, 20*time.Second, "scheduler entries visible", func() bool {
		entries, err := insp.SchedulerEntries()
		return err == nil && len(entries) == 3
	})
	// Outcome material: a retained completion, a parked retry, and at least
	// one processed (and therefore deleted) retention-0 task.
	schedWaitFor(t, 20*time.Second, "sq outcomes (completed + retry)", func() bool {
		qi, err := insp.GetQueueInfo("sq")
		return err == nil && qi.Completed >= 1 && qi.Retry >= 1
	})
	schedWaitFor(t, 20*time.Second, "default-queue retention-0 completion", func() bool {
		qi, err := insp.GetQueueInfo("default")
		return err == nil && qi.Processed >= 1
	})

	goneAfter := 300 * time.Millisecond
	engine := stats.NewEngine(stats.Config{
		RedisClient:        rc,
		Inspector:          insp,
		SchedulerGoneAfter: goneAfter,
		Logf:               func(string, ...interface{}) {},
	})
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return &schedFixture{rc: rc, insp: insp, scheduler: scheduler, server: srv, engine: engine, goneAfter: goneAfter}
}

// schedRouter mounts both handlers under mux so {stable_key} resolves.
func schedRouter(f *schedFixture) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/api/schedulers", newListSchedulersHandlerFunc(f.insp, f.rc, DefaultPayloadFormatter, f.goneAfter)).Methods("GET")
	r.HandleFunc("/api/schedulers/{stable_key}/outcomes", newSchedulerOutcomesHandlerFunc(f.insp, f.rc)).Methods("GET")
	return r
}

func getJSONRouter(t *testing.T, r *mux.Router, url string, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if out != nil && w.Code == http.StatusOK {
		if err := jsonUnmarshalBody(w, out); err != nil {
			t.Fatalf("decoding %s response: %v\nbody: %s", url, err, w.Body.String())
		}
	}
	return w
}

func rowByType(resp listSchedulersResponse, taskType string) *schedulerRow {
	for _, row := range resp.Entries {
		if row.Entry != nil && row.Entry.TaskType == taskType {
			return row
		}
	}
	return nil
}

func outcomesOf(resp schedulerOutcomesResponse, outcome string) []*schedulerOutcomeEvent {
	var out []*schedulerOutcomeEvent
	for _, ev := range resp.Events {
		if ev.Outcome == outcome {
			out = append(out, ev)
		}
	}
	return out
}

// TestSchedulersEndToEnd drives the whole §3.8 story on one fixture: merged
// live rows, the three-valued outcome joins, 404s, and — as the final,
// mutating act — the stop-scheduler → SCHEDULER GONE transition.
func TestSchedulersEndToEnd(t *testing.T) {
	f := newSchedFixture(t)
	router := schedRouter(f)
	ctx := context.Background()

	Convey("Given a live scheduler (3 entries), a working server, and one sweep", t, func() {
		var list listSchedulersResponse
		w := getJSONRouter(t, router, "/api/schedulers", &list)
		So(w.Code, ShouldEqual, http.StatusOK)
		So(list.Entries, ShouldHaveLength, 3)

		Convey("When GET /api/schedulers is served", func() {
			Convey("Then every row is live with a stable key and snapshot stamps", func() {
				for _, row := range list.Entries {
					So(row.Live, ShouldBeTrue)
					So(row.StableKey, ShouldHaveLength, 64)
					So(row.GoneSince, ShouldBeEmpty)
					// The sweep persisted each entry before the request.
					So(row.FirstSeen, ShouldNotBeEmpty)
					So(row.LastSeen, ShouldNotBeEmpty)
				}
				// Sorted by task type.
				So(list.Entries[0].Entry.TaskType, ShouldEqual, "sched:fail")
				So(list.Entries[1].Entry.TaskType, ShouldEqual, "sched:noret")
				So(list.Entries[2].Entry.TaskType, ShouldEqual, "sched:ok")
			})

			Convey("Then rows carry the legacy entry shape (spec, options, next/prev)", func() {
				ok := rowByType(list, "sched:ok")
				So(ok, ShouldNotBeNil)
				So(ok.Entry.ID, ShouldNotBeEmpty)
				So(ok.Entry.Spec, ShouldEqual, "@every 1s")
				So(ok.Entry.Opts, ShouldContain, `Queue("sq")`)
				So(ok.Entry.Opts, ShouldContain, "Retention(1h0m0s)")
				So(ok.Entry.NextEnqueueAt, ShouldNotBeEmpty)
			})
		})

		Convey("When outcomes are joined for the retained, succeeding entry", func() {
			ok := rowByType(list, "sched:ok")
			So(ok, ShouldNotBeNil)
			var resp schedulerOutcomesResponse
			w := getJSONRouter(t, router, "/api/schedulers/"+ok.StableKey+"/outcomes", &resp)

			Convey("Then completed-state tasks are found and labeled succeeded", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(resp.Queue, ShouldEqual, "sq")
				So(resp.RetentionSeconds, ShouldEqual, 3600)
				So(resp.HistoryCap, ShouldEqual, 1000)
				So(resp.Events, ShouldNotBeEmpty)
				succeeded := outcomesOf(resp, "succeeded")
				So(succeeded, ShouldNotBeEmpty)
				So(succeeded[0].State, ShouldEqual, "completed")
				So(succeeded[0].TaskID, ShouldNotBeEmpty)
				So(succeeded[0].Reason, ShouldBeEmpty)
			})
		})

		Convey("When outcomes are joined for the failing entry", func() {
			fail := rowByType(list, "sched:fail")
			So(fail, ShouldNotBeNil)
			var resp schedulerOutcomesResponse
			getJSONRouter(t, router, "/api/schedulers/"+fail.StableKey+"/outcomes", &resp)

			Convey("Then retry-state tasks with retries count as failed", func() {
				So(resp.RetentionSeconds, ShouldEqual, 0)
				failed := outcomesOf(resp, "failed")
				So(failed, ShouldNotBeEmpty)
				So(failed[0].State, ShouldEqual, "retry")
			})
		})

		Convey("When outcomes are joined for the retention-0 entry", func() {
			noret := rowByType(list, "sched:noret")
			So(noret, ShouldNotBeNil)
			var resp schedulerOutcomesResponse
			getJSONRouter(t, router, "/api/schedulers/"+noret.StableKey+"/outcomes", &resp)

			Convey("Then deleted completions are unknown with the honest reason, and the response carries retention 0 for the UI guidance", func() {
				So(resp.Queue, ShouldEqual, "default")
				So(resp.RetentionSeconds, ShouldEqual, 0)
				unknown := outcomesOf(resp, "unknown")
				So(unknown, ShouldNotBeEmpty)
				So(unknown[0].Reason, ShouldEqual, "not_found — task deleted or Retention=0")
				So(unknown[0].State, ShouldBeEmpty)
			})
		})

		Convey("When an unknown stable key is requested", func() {
			w := getJSONRouter(t, router, "/api/schedulers/deadbeef/outcomes", nil)
			Convey("Then it is a 404, not an empty 200", func() {
				So(w.Code, ShouldEqual, http.StatusNotFound)
			})
		})

		// Keep this leaf last: it kills the scheduler.
		Convey("When the scheduler process stops and the gone threshold passes", func() {
			f.scheduler.Shutdown()
			schedWaitFor(t, 10*time.Second, "entries cleared from redis", func() bool {
				entries, err := f.insp.SchedulerEntries()
				return err == nil && len(entries) == 0
			})
			time.Sleep(f.goneAfter + 200*time.Millisecond)
			So(f.engine.SweepNow(ctx), ShouldBeNil)

			Convey("Then the rows survive as snapshots flagged SCHEDULER GONE", func() {
				var after listSchedulersResponse
				w := getJSONRouter(t, router, "/api/schedulers", &after)
				So(w.Code, ShouldEqual, http.StatusOK)
				So(after.Entries, ShouldHaveLength, 3)
				for _, row := range after.Entries {
					So(row.Live, ShouldBeFalse)
					So(row.GoneSince, ShouldNotBeEmpty)
					So(row.GoneSince, ShouldEqual, row.LastSeen)
					So(row.Entry.TaskType, ShouldNotBeEmpty) // rebuilt from the snapshot
				}
			})

			Convey("Then the attention engine raises detector #9 for each entry", func() {
				rep, err := f.engine.ReadAttention(ctx)
				So(err, ShouldBeNil)
				var gone []stats.AttentionFinding
				for _, finding := range rep.Findings {
					if finding.Detector == stats.DetectorSchedulerGone {
						gone = append(gone, finding)
					}
				}
				So(gone, ShouldHaveLength, 3)
				byType := map[string]stats.AttentionFinding{}
				for _, g := range gone {
					// SuggestedQuery targets the Schedulers screen.
					So(g.SuggestedQuery, ShouldStartWith, "schedulers type=")
					byType[g.SuggestedQuery] = g
				}
				ok := byType["schedulers type=sched:ok"]
				So(ok.Severity, ShouldEqual, 4)
				So(ok.Queue, ShouldEqual, "sq") // the entry's TARGET queue
				So(ok.Sentence, ShouldContainSubstring, "entry sched:ok last seen")
				So(ok.Sentence, ShouldContainSubstring, "scheduler process gone or entry removed")
				So(ok.Chips, ShouldResemble, []string{"SCHEDULER GONE"})
				So(byType["schedulers type=sched:noret"].Queue, ShouldEqual, "default")
			})

			Convey("And when a new scheduler process registers the same entry", func() {
				opt := asynq.RedisClientOpt{Addr: schedTestRedisAddr, DB: schedTestRedisDB}
				sched2 := asynq.NewScheduler(opt, &asynq.SchedulerOpts{LogLevel: asynq.FatalLevel})
				_, err := sched2.Register("@every 1s", asynq.NewTask("sched:ok", []byte(`{"fixture":true}`)),
					asynq.Queue("sq"), asynq.Retention(time.Hour))
				So(err, ShouldBeNil)
				So(sched2.Start(), ShouldBeNil)
				Reset(sched2.Shutdown)
				schedWaitFor(t, 20*time.Second, "restarted scheduler visible", func() bool {
					entries, err := f.insp.SchedulerEntries()
					return err == nil && len(entries) == 1
				})
				So(f.engine.SweepNow(ctx), ShouldBeNil)

				Convey("Then the row is live again under the SAME stable key with the ID history grown", func() {
					var again listSchedulersResponse
					getJSONRouter(t, router, "/api/schedulers", &again)
					okBefore := rowByType(list, "sched:ok")
					okAfter := rowByType(again, "sched:ok")
					So(okAfter, ShouldNotBeNil)
					So(okAfter.StableKey, ShouldEqual, okBefore.StableKey) // identity survived the restart
					So(okAfter.Live, ShouldBeTrue)
					So(okAfter.GoneSince, ShouldBeEmpty)
					So(okAfter.FirstSeen, ShouldEqual, okBefore.FirstSeen) // history, not a fresh row
					So(okAfter.Entry.ID, ShouldNotEqual, okBefore.Entry.ID) // ephemeral ID changed

					snaps, err := stats.ReadSchedulerSnapshots(ctx, f.rc)
					So(err, ShouldBeNil)
					var snap *stats.SchedulerSnapshot
					for _, s := range snaps {
						if s.StableKey == okAfter.StableKey {
							snap = s
						}
					}
					So(snap, ShouldNotBeNil)
					So(len(snap.EntryIDs), ShouldEqual, 2) // both incarnations remembered
					So(snap.EntryIDs[0], ShouldEqual, okAfter.Entry.ID)

					Convey("And detector #9 auto-clears for the re-registered entry", func() {
						rep, err := f.engine.ReadAttention(ctx)
						So(err, ShouldBeNil)
						for _, finding := range rep.Findings {
							So(finding.SuggestedQuery, ShouldNotEqual, "schedulers type=sched:ok")
						}
					})
				})
			})
		})
	})
}

// jsonUnmarshalBody decodes a recorder body into out.
func jsonUnmarshalBody(w *httptest.ResponseRecorder, out interface{}) error {
	return json.Unmarshal(w.Body.Bytes(), out)
}
