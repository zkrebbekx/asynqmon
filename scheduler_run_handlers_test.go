package asynqmon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Integration tests for POST /api/schedulers/{stable_key}/run (upstream
// hibiken/asynqmon#337) against a real Redis on 127.0.0.1:6379 DB 10 — the
// scheduler suite's DB; only DB 10 is flushed here. Skipped when Redis does
// not answer PING.
//
// Fixtures are authentic: a real asynq.Scheduler registers the entries (its
// heartbeat writes them, ~5s), the stats engine's sweep persists the §5.12
// snapshots, and the GONE flow really shuts the scheduler down. Entry specs
// are "@every 1h" so the scheduler itself never enqueues during the test —
// every task observed was created by the run-now endpoint.
//
// Convey discipline: every mutating flow runs imperatively BEFORE the Convey
// tree; the tree only reads captured results.
// ****************************************************************************

const (
	runTestRedisAddr = "127.0.0.1:6379"
	runTestRedisDB   = 10
)

type runNowResponse struct {
	Task           *taskInfo `json:"task"`
	AppliedOptions []string  `json:"applied_options"`
	SkippedOptions []string  `json:"skipped_options"`
}

// runFixture is the shared end-to-end environment for the run-now suite.
type runFixture struct {
	rc        *redis.Client
	client    *asynq.Client
	insp      *asynq.Inspector
	store     *jobs.Store
	scheduler *asynq.Scheduler
	engine    *stats.Engine
	goneAfter time.Duration
}

// newRunFixture flushes DB 10 and starts a scheduler with three entries:
//
//	run:rich   @every 1h  Queue("runq") MaxRetry(5) Timeout(2m) Retention(1h) ProcessIn(90s)
//	run:plain  @every 1h  (no options — default queue)
//	run:uniq   @every 1h  Queue("runq") Unique(1h)
//
// then waits for the heartbeat and persists one sweep.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	ctx := context.Background()

	rc := redis.NewClient(&redis.Options{Addr: runTestRedisAddr, DB: runTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", runTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", runTestRedisDB, err)
	}
	t.Cleanup(func() { rc.Close() })

	opt := asynq.RedisClientOpt{Addr: runTestRedisAddr, DB: runTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
	})

	scheduler := asynq.NewScheduler(opt, &asynq.SchedulerOpts{LogLevel: asynq.FatalLevel})
	register := func(spec, typ string, opts ...asynq.Option) {
		t.Helper()
		if _, err := scheduler.Register(spec, asynq.NewTask(typ, []byte(`{"fixture":"run-now"}`)), opts...); err != nil {
			t.Fatalf("registering %s: %v", typ, err)
		}
	}
	register("@every 1h", "run:rich",
		asynq.Queue("runq"), asynq.MaxRetry(5), asynq.Timeout(2*time.Minute),
		asynq.Retention(time.Hour), asynq.ProcessIn(90*time.Second))
	register("@every 1h", "run:plain")
	register("@every 1h", "run:uniq", asynq.Queue("runq"), asynq.Unique(time.Hour))
	if err := scheduler.Start(); err != nil {
		t.Fatalf("starting scheduler: %v", err)
	}
	t.Cleanup(scheduler.Shutdown) // idempotent; GONE flow shuts down early

	schedWaitFor(t, 20*time.Second, "scheduler entries visible", func() bool {
		entries, err := insp.SchedulerEntries()
		return err == nil && len(entries) == 3
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
	return &runFixture{
		rc: rc, client: client, insp: insp, store: jobs.NewStore(rc),
		scheduler: scheduler, engine: engine, goneAfter: goneAfter,
	}
}

// newRouter builds the real router (§5.10 route block, actor middleware,
// read-only filter included) with the fixture's asynq client injected as the
// enqueue client.
func (f *runFixture) newRouter(opts Options) http.Handler {
	opts.enqueueClient = f.client
	return muxRouter(opts, f.rc, f.insp, nil, nil)
}

// stableKeysByType reads GET /api/schedulers and indexes stable keys by type.
func stableKeysByType(t *testing.T, router http.Handler) map[string]string {
	t.Helper()
	var list listSchedulersResponse
	w := doJSON(t, router, "GET", "/api/schedulers", nil, &list)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/schedulers: %d %s", w.Code, w.Body.String())
	}
	out := make(map[string]string, len(list.Entries))
	for _, row := range list.Entries {
		if row.Entry != nil {
			out[row.Entry.TaskType] = row.StableKey
		}
	}
	return out
}

func runURL(stableKey string) string { return "/api/schedulers/" + stableKey + "/run" }

// ----------------------------------------------------------------------------
// Live entry: options reconstructed and applied, schedule opts skipped and
// declared, audit written. Unique entries 409 on the second manual run.
// ----------------------------------------------------------------------------

func TestRunSchedulerEntryNow(t *testing.T) {
	f := newRunFixture(t)
	router := f.newRouter(Options{EnableEnqueue: true})
	keys := stableKeysByType(t, router)

	// Rich entry: full option set, ProcessIn deliberately present so the
	// skip ledger has something honest to say.
	var rich runNowResponse
	wRich := doJSON(t, router, "POST", runURL(keys["run:rich"]), map[string]interface{}{
		"reason": "manual catch-up",
	}, &rich)
	var richInspected *asynq.TaskInfo
	var richInspectErr error
	if wRich.Code == http.StatusCreated && rich.Task != nil {
		richInspected, richInspectErr = f.insp.GetTaskInfo("runq", rich.Task.ID)
	}

	// Unique entry: the first manual run takes the uniqueness lock, the
	// identical second must answer 409.
	var uniq runNowResponse
	wUniq1 := doJSON(t, router, "POST", runURL(keys["run:uniq"]), nil, &uniq)
	wUniq2 := doJSON(t, router, "POST", runURL(keys["run:uniq"]), nil, nil)

	// Unknown stable key.
	wUnknown := doJSON(t, router, "POST", runURL("deadbeef"), nil, nil)

	audits, auditErr := f.store.ReadAudit(context.Background(), 10)
	var richAudit *jobs.AuditEntry
	for i := range audits {
		if rich.Task != nil && audits[i].TaskID == rich.Task.ID {
			richAudit = &audits[i]
		}
	}

	Convey("Given a live scheduler and an enqueue-enabled router (#337)", t, func() {
		Convey("When the rich entry is run now", func() {
			Convey("Then it answers 201 with the created task in the entry's target queue", func() {
				So(wRich.Code, ShouldEqual, http.StatusCreated)
				So(rich.Task, ShouldNotBeNil)
				So(rich.Task.ID, ShouldNotBeEmpty)
				So(rich.Task.Queue, ShouldEqual, "runq")
				So(rich.Task.Type, ShouldEqual, "run:rich")
				So(rich.Task.Payload, ShouldEqual, `{"fixture":"run-now"}`)
				// ProcessIn was skipped — the task is due NOW, not in 90s.
				So(rich.Task.State, ShouldEqual, "pending")
			})
			Convey("Then the Inspector agrees on every applied option", func() {
				So(richInspectErr, ShouldBeNil)
				So(richInspected, ShouldNotBeNil)
				So(richInspected.Type, ShouldEqual, "run:rich")
				So(string(richInspected.Payload), ShouldEqual, `{"fixture":"run-now"}`)
				So(richInspected.MaxRetry, ShouldEqual, 5)
				So(richInspected.Timeout, ShouldEqual, 2*time.Minute)
				So(richInspected.Retention, ShouldEqual, time.Hour)
				So(richInspected.State, ShouldEqual, asynq.TaskStatePending)
			})
			Convey("Then the response declares applied and skipped options honestly", func() {
				So(rich.AppliedOptions, ShouldContain, `Queue("runq")`)
				So(rich.AppliedOptions, ShouldContain, "MaxRetry(5)")
				So(rich.AppliedOptions, ShouldContain, "Timeout(2m0s)")
				So(rich.AppliedOptions, ShouldContain, "Retention(1h0m0s)")
				So(rich.SkippedOptions, ShouldHaveLength, 1)
				So(rich.SkippedOptions[0], ShouldEqual, "ProcessIn(1m30s) — schedule is replaced by run-now")
			})
			Convey("Then an attributed audit entry carries the #337 verb, scope, and task id", func() {
				So(auditErr, ShouldBeNil)
				So(richAudit, ShouldNotBeNil)
				So(richAudit.Event, ShouldEqual, jobs.AuditTaskEnqueued)
				So(richAudit.Verb, ShouldEqual, "run_scheduler_entry")
				So(richAudit.Actor, ShouldStartWith, "anonymous@")
				So(richAudit.Scope.Queue, ShouldEqual, "runq")
				So(richAudit.Scope.Q, ShouldEqual, "run:rich")
				So(richAudit.TaskID, ShouldEqual, rich.Task.ID)
				So(richAudit.Acted, ShouldEqual, 1)
				So(richAudit.Reason, ShouldEqual, "manual catch-up")
			})
		})

		Convey("When a Unique entry is run twice inside its TTL", func() {
			Convey("Then the first run is created with Unique applied and the second answers 409", func() {
				So(wUniq1.Code, ShouldEqual, http.StatusCreated)
				So(uniq.AppliedOptions, ShouldContain, "Unique(1h0m0s)")
				So(wUniq2.Code, ShouldEqual, http.StatusConflict)
				So(wUniq2.Body.String(), ShouldContainSubstring, "uniqueness lock")
			})
		})

		Convey("When the stable key is unknown", func() {
			Convey("Then it is a 404, not an empty enqueue", func() {
				So(wUnknown.Code, ShouldEqual, http.StatusNotFound)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// GONE entry: the snapshot alone is enough to fire the entry manually — the
// precise reason this endpoint exists. Runs its own fixture because it kills
// the scheduler.
// ----------------------------------------------------------------------------

func TestRunSchedulerEntryNowWhenGone(t *testing.T) {
	f := newRunFixture(t)
	ctx := context.Background()
	router := f.newRouter(Options{EnableEnqueue: true})
	keys := stableKeysByType(t, router)

	// Kill the scheduler and sweep: the entries vanish from asynq's live set
	// and survive only as §5.12 snapshots — the exact state a SCHEDULER GONE
	// row is in (the production router flags GONE after
	// stats.DefaultSchedulerGoneAfter, 30s; run-now resolves the snapshot
	// either way, so this test does not wait the threshold out).
	f.scheduler.Shutdown()
	schedWaitFor(t, 10*time.Second, "entries cleared from redis", func() bool {
		entries, err := f.insp.SchedulerEntries()
		return err == nil && len(entries) == 0
	})
	time.Sleep(f.goneAfter + 200*time.Millisecond)
	if err := f.engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var after listSchedulersResponse
	doJSON(t, router, "GET", "/api/schedulers", nil, &after)
	notLiveCount := 0
	for _, row := range after.Entries {
		if !row.Live {
			notLiveCount++
		}
	}

	// Fire the default-queue entry purely from its snapshot.
	var plain runNowResponse
	wPlain := doJSON(t, router, "POST", runURL(keys["run:plain"]), nil, &plain)
	var plainInspected *asynq.TaskInfo
	if wPlain.Code == http.StatusCreated && plain.Task != nil {
		plainInspected, _ = f.insp.GetTaskInfo("default", plain.Task.ID)
	}

	Convey("Given a dead scheduler whose entries survive only as snapshots (#337)", t, func() {
		Convey("When the rows are re-read after the sweep", func() {
			Convey("Then every row is snapshot-only — no live counterpart", func() {
				So(after.Entries, ShouldHaveLength, 3)
				So(notLiveCount, ShouldEqual, 3)
			})
		})
		Convey("When a snapshot-only (GONE-track) entry is run now", func() {
			Convey("Then the task is created with the snapshot's type, payload, and target queue", func() {
				So(wPlain.Code, ShouldEqual, http.StatusCreated)
				So(plain.Task, ShouldNotBeNil)
				So(plain.Task.Queue, ShouldEqual, "default")
				So(plain.Task.Type, ShouldEqual, "run:plain")
				So(plain.AppliedOptions, ShouldContain, `Queue("default")`)
				So(plain.SkippedOptions, ShouldBeEmpty)
				So(plainInspected, ShouldNotBeNil)
				So(string(plainInspected.Payload), ShouldEqual, `{"fixture":"run-now"}`)
				So(plainInspected.State, ShouldEqual, asynq.TaskStatePending)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Gating: flag off → 403 pointing at --enable-enqueue; read-only → 403 naming
// read-only mode; both JSON, exactly like the §5.10 enqueue route. No Redis
// fixture needed beyond a reachable client — no scheduler is required to be
// refused.
// ----------------------------------------------------------------------------

func TestRunSchedulerEntryGating(t *testing.T) {
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: runTestRedisAddr, DB: runTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", runTestRedisAddr, err)
	}
	t.Cleanup(func() { rc.Close() })
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: runTestRedisAddr, DB: runTestRedisDB})
	t.Cleanup(func() { insp.Close() })

	disabledRouter := muxRouter(Options{}, rc, insp, nil, nil)
	wDisabled := doJSON(t, disabledRouter, "POST", runURL("somekey"), nil, nil)

	roRouter := muxRouter(Options{EnableEnqueue: true, ReadOnly: true}, rc, insp, nil, nil)
	wRO := doJSON(t, roRouter, "POST", runURL("somekey"), nil, nil)

	Convey("Given routers with the enqueue capability gated off (#337)", t, func() {
		Convey("When run-now is POSTed without --enable-enqueue", func() {
			Convey("Then it answers 403 JSON pointing at the flag", func() {
				So(wDisabled.Code, ShouldEqual, http.StatusForbidden)
				So(wDisabled.Header().Get("Content-Type"), ShouldContainSubstring, "application/json")
				So(wDisabled.Body.String(), ShouldContainSubstring, "--enable-enqueue")
			})
		})
		Convey("When run-now is POSTed against a read-only replica", func() {
			Convey("Then it answers 403 JSON naming read-only mode, not a bare 405", func() {
				So(wRO.Code, ShouldEqual, http.StatusForbidden)
				So(wRO.Header().Get("Content-Type"), ShouldContainSubstring, "application/json")
				So(wRO.Body.String(), ShouldContainSubstring, "read-only")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Option reconstruction is pure — no Redis. Unparsable and unsupported
// options are skipped WITH reasons; the enqueue still proceeds on queue +
// whatever parsed (the #337 "unparsable opts → queue + defaults, and say so"
// contract).
// ----------------------------------------------------------------------------

func TestBuildRunNowOptions(t *testing.T) {
	richOpts, richApplied, richSkipped := buildRunNowOptions(stats.SchedulerEntryData{
		TaskType: "t", Queue: "runq",
		Opts: []string{
			`Queue("runq")`, "MaxRetry(5)", "Timeout(2m0s)", "Retention(1h0m0s)",
			"Unique(30m0s)", "ProcessIn(1m30s)",
		},
	})

	messyOpts, messyApplied, messySkipped := buildRunNowOptions(stats.SchedulerEntryData{
		TaskType: "t", Queue: "",
		Opts: []string{
			"garbage",                // no parens at all
			"MaxRetry(not-a-number)", // parses as an int? no — skipped
			"Deadline(not a date)",   // malformed UnixDate
			`TaskID("fixed-id")`,     // would collide with scheduler enqueues
			`Group("g")`,             // aggregation contradicts run-now
			"Frobnicate(3)",          // unknown option from a future asynq
			"Timeout(45s)",           // the one salvageable option
		},
	})

	Convey("Given buildRunNowOptions over entry option strings (#337)", t, func() {
		Convey("When every option is parsable", func() {
			Convey("Then queue/retry/timeout/retention/unique apply and only the schedule is skipped", func() {
				So(len(richOpts), ShouldEqual, 5) // Queue + 4 applied
				So(richApplied, ShouldResemble, []string{
					`Queue("runq")`, "MaxRetry(5)", "Timeout(2m0s)", "Retention(1h0m0s)", "Unique(30m0s)",
				})
				So(richSkipped, ShouldResemble, []string{"ProcessIn(1m30s) — schedule is replaced by run-now"})
			})
		})
		Convey("When options are unparsable or unsupported", func() {
			Convey("Then the run degrades to queue + defaults and each skip names its reason", func() {
				So(len(messyOpts), ShouldEqual, 2) // Queue + Timeout
				So(messyApplied, ShouldResemble, []string{`Queue("default")`, "Timeout(45s)"})
				So(messySkipped, ShouldResemble, []string{
					"garbage — unparsable",
					"MaxRetry(not-a-number) — unparsable",
					"Deadline(not a date) — unparsable",
					`TaskID("fixed-id") — a fresh task id is assigned`,
					`Group("g") — grouping is not applied on a manual run`,
					"Frobnicate(3) — unsupported option",
				})
			})
		})
	})
}
