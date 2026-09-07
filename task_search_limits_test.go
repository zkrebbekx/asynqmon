package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests for the phase "scan limits" review fixes (#27, #31, #39,
// #44, #47, #48, #54): streaming scans, the process-wide scan gate, the
// shared batch_filtered limiter, the truncated-at-budget rule, the
// state_counts fallback, the clamped page size, and the pending_since gap.
//
// Redis: DB 14 on the address ASYNQMON_TEST_REDIS_ADDR names (no other suite
// uses DB 14). Every env creation flushes it.
//
// Convey discipline: flows that touch Redis run imperatively BEFORE the
// Convey tree; the tree only reads captured results.
// ****************************************************************************

const limitsTestRedisDB = 14

type limitsTestEnv struct {
	rc     *redis.Client
	client *asynq.Client
	insp   *asynq.Inspector
	opt    asynq.RedisClientOpt
}

// newLimitsTestEnv flushes DB 14 and returns clients wired to it.
func newLimitsTestEnv(t *testing.T) *limitsTestEnv {
	t.Helper()
	ctx := context.Background()
	addr := testRedisAddrFromEnv()
	rc := redis.NewClient(&redis.Options{Addr: addr, DB: limitsTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", addr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	opt := asynq.RedisClientOpt{Addr: addr, DB: limitsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})
	return &limitsTestEnv{rc: rc, client: client, insp: insp, opt: opt}
}

func (e *limitsTestEnv) router(opts Options) http.Handler {
	opts.RedisConnOpt = e.opt
	return muxRouter(opts, e.rc, e.insp, nil, nil)
}

// seed enqueues n pending tasks into qname.
func (e *limitsTestEnv) seed(t *testing.T, qname, typ, payload string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.client.Enqueue(asynq.NewTask(typ, []byte(payload)), asynq.Queue(qname)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
}

func limitsGet(t *testing.T, h http.Handler, rawURL string, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", rawURL, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding GET %s (status %d): %v\nbody: %s", rawURL, w.Code, err, w.Body.String())
		}
	}
	return w
}

// ----------------------------------------------------------------------------
// #27 — the scan gate: ceiling, 429, release
// ----------------------------------------------------------------------------

func TestScanGateRefusesWhenFull(t *testing.T) {
	env := newLimitsTestEnv(t)
	env.seed(t, "gateq", "gate:x", `{"n":1}`, 5)

	gate := newScanGate(1, 20000)
	legacy := newSearchTasksHandlerFunc(env.insp, env.rc, nil, DefaultPayloadFormatter, gate)
	aggregate := newTaskAggregateHandlerFunc(env.insp, env.rc, DefaultPayloadFormatter, gate)
	metadata := newTaskMetadataHandlerFunc(env.insp, env.rc, DefaultPayloadFormatter, gate)

	// Hold the only slot, exactly as an in-flight scan would.
	if !gate.tryAcquire() {
		t.Fatal("fresh gate refused the first slot")
	}
	serve := func(h http.HandlerFunc, url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		h(w, req)
		return w
	}
	wLegacy := serve(legacy, "/api/tasks?queue=gateq&state=pending")
	wAql := serve(legacy, "/api/tasks?q=state%3Dpending+meta.n%3D1")
	wAgg := serve(aggregate, "/api/task_aggregate?queue=gateq&state=pending")
	wMeta := serve(metadata, "/api/task_metadata?queue=gateq&state=pending")
	gate.release()
	wAfter := serve(legacy, "/api/tasks?queue=gateq&state=pending")

	Convey("Given a scan gate with one slot, already held", t, func() {
		Convey("When a scan request arrives on any scan endpoint", func() {
			Convey("Then it is refused with 429 and Retry-After: 1", func() {
				for _, w := range []*httptest.ResponseRecorder{wLegacy, wAql, wAgg, wMeta} {
					So(w.Code, ShouldEqual, http.StatusTooManyRequests)
					So(w.Header().Get("Retry-After"), ShouldEqual, "1")
					So(w.Body.String(), ShouldContainSubstring, "too many concurrent scans")
				}
			})
		})
		Convey("When the slot is released", func() {
			Convey("Then the next scan runs normally", func() {
				So(wAfter.Code, ShouldEqual, http.StatusOK)
			})
		})
	})
}

func TestScanGateCeiling(t *testing.T) {
	Convey("Given a scan gate built from the Options defaults", t, func() {
		gate := newScanGate(0, 0)
		Convey("Then max_scan clamps to MaxScanCeiling, not to the old 100000", func() {
			So(gate.ceiling, ShouldEqual, defaultMaxScanCeiling)
			So(gate.clampMaxScan(100000), ShouldEqual, 20000)
			So(gate.clampMaxScan(0), ShouldEqual, defaultMaxScan)
			So(gate.clampMaxScan(500), ShouldEqual, 500)
		})
		Convey("Then an explicit ceiling below the default budget still holds", func() {
			small := newScanGate(2, 100)
			So(small.clampMaxScan(0), ShouldEqual, 100)
			So(small.clampMaxScan(5000), ShouldEqual, 100)
		})
	})
}

// ----------------------------------------------------------------------------
// #27 — memory: two scans over 50k tasks stay inside a heap budget
// ----------------------------------------------------------------------------

// scanHeapBudget is the extra live heap two full 50k-task scans may hold.
//
// The streaming scan keeps one listing batch (1000 TaskInfo values plus their
// formatted searchTask rows, roughly 1 KiB each => about 2 MiB) plus one page
// window (20 rows) and the counters. Two concurrent scans therefore need
// about 4 MiB. 32 MiB leaves room for the Redis reply buffers and the
// per-batch garbage the collector has not swept yet, while still failing hard
// on the old behaviour: 50k formatted rows are about 50 MiB per scan, 100 MiB
// for two.
const scanHeapBudget = 32 << 20

func TestScanMemoryStaysBounded(t *testing.T) {
	env := newLimitsTestEnv(t)

	// Seeding 50k tasks through the real client is the honest fixture, but it
	// is also the slow part; give up rather than hang a CI run.
	const want = 50000
	deadline := time.Now().Add(60 * time.Second)
	seeded := 0
	for ; seeded < want; seeded++ {
		if seeded%1000 == 0 && time.Now().After(deadline) {
			break
		}
		if _, err := env.client.Enqueue(asynq.NewTask("mem:x", []byte(`{"region":"eu","n":1}`)), asynq.Queue("memq")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if seeded < want {
		t.Skipf("skipping: seeded only %d of %d tasks before the 60s budget", seeded, want)
	}

	h := env.router(Options{MaxConcurrentScans: 4, MaxScanCeiling: 100000})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	type resp struct {
		Total   int64 `json:"total"`
		Scanned int   `json:"scanned"`
		Tasks   []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	var a, b resp
	// Two scans over every seeded task, one after the other; each asks for a
	// 20-row page, so only the page window may survive.
	wA := limitsGet(t, h, "/api/tasks?queue=memq&state=pending&size=20&max_scan=60000", &a)
	wB := limitsGet(t, h, "/api/tasks?queue=memq&state=pending&size=20&max_scan=60000&page=2", &b)
	runtime.ReadMemStats(&after)
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	Convey("Given 50000 pending tasks and two full scans over them", t, func() {
		Convey("When both scans complete", func() {
			Convey("Then each reports every task, with only one page of rows", func() {
				So(wA.Code, ShouldEqual, 200)
				So(wB.Code, ShouldEqual, 200)
				So(a.Total, ShouldEqual, int64(want))
				So(a.Scanned, ShouldEqual, want)
				So(len(a.Tasks), ShouldEqual, 20)
				So(len(b.Tasks), ShouldEqual, 20)
			})
			Convey("Then the live heap grew by less than the scan budget", func() {
				t.Logf("HeapAlloc before=%d after=%d delta=%d bytes (budget %d)",
					before.HeapAlloc, after.HeapAlloc, delta, int64(scanHeapBudget))
				So(delta, ShouldBeLessThan, int64(scanHeapBudget))
			})
		})
	})
}

// ----------------------------------------------------------------------------
// #31 — batch_filtered: the 2000-match cap and the shared limiter
// ----------------------------------------------------------------------------

func TestBatchFilteredRefusesLargeMatchSets(t *testing.T) {
	env := newLimitsTestEnv(t)
	const seeded = batchFilteredMaxMatches + 100
	env.seed(t, "bigq", "bulk:x", `{"n":1}`, seeded)
	h := env.router(Options{})

	body := fmt.Sprintf(`{"queue":"bigq","state":"pending","action":"delete","max_scan":%d}`, 20000)
	req := httptest.NewRequest("POST", "/api/tasks:batch_filtered", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, req)
	elapsed := time.Since(start)

	left, err := env.insp.GetQueueInfo("bigq")
	if err != nil {
		t.Fatalf("reading queue info: %v", err)
	}

	Convey("Given a filter matching more tasks than the synchronous cap", t, func() {
		Convey("When batch_filtered runs it", func() {
			Convey("Then it is refused with 400 and pointed at the jobs API", func() {
				So(w.Code, ShouldEqual, http.StatusBadRequest)
				So(w.Body.String(), ShouldContainSubstring, "POST /api/jobs")
				So(w.Body.String(), ShouldContainSubstring, fmt.Sprintf("more than %d tasks", batchFilteredMaxMatches))
			})
			Convey("Then it answers in under a second and deletes nothing", func() {
				So(elapsed, ShouldBeLessThan, time.Second)
				So(left.Pending, ShouldEqual, seeded)
			})
		})
	})
}

func TestBatchFilteredLimiterIsProcessWide(t *testing.T) {
	Convey("Given the batch_filtered throttle", t, func() {
		Convey("Then it is one package-level limiter at the jobs runner's rate", func() {
			So(float64(batchFilteredLimiter.Limit()), ShouldEqual, float64(batchFilteredActionsPerSecond))
			So(batchFilteredLimiter.Burst(), ShouldEqual, batchFilteredActionsPerSecond/10)
		})
	})
}

// ----------------------------------------------------------------------------
// #54.5 — truncated only when the budget stopped a scan with work left
// ----------------------------------------------------------------------------

func TestLegacyScanTruncatedOnlyWhenWorkRemains(t *testing.T) {
	env := newLimitsTestEnv(t)
	const seeded = 1200 // one full 1000-task batch plus a short one
	env.seed(t, "truncq", "trunc:x", `{"n":1}`, seeded)
	h := env.router(Options{})

	type resp struct {
		Total     int64 `json:"total"`
		Scanned   int   `json:"scanned"`
		Truncated bool  `json:"truncated"`
	}
	var exact, short resp
	wExact := limitsGet(t, h, "/api/tasks?queue=truncq&state=pending&max_scan=1200", &exact)
	wShort := limitsGet(t, h, "/api/tasks?queue=truncq&state=pending&max_scan=1000", &short)

	Convey("Given 1200 pending tasks in one queue", t, func() {
		Convey("When the budget equals the queue size", func() {
			Convey("Then the completed scan is not reported as truncated", func() {
				So(wExact.Code, ShouldEqual, 200)
				So(exact.Scanned, ShouldEqual, seeded)
				So(exact.Total, ShouldEqual, int64(seeded))
				So(exact.Truncated, ShouldBeFalse)
			})
		})
		Convey("When the budget stops the scan with tasks left", func() {
			Convey("Then it is reported as truncated", func() {
				So(wShort.Code, ShouldEqual, 200)
				So(short.Scanned, ShouldEqual, 1000)
				So(short.Truncated, ShouldBeTrue)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// #44 — state_counts falls back to the per-state keys
// ----------------------------------------------------------------------------

func TestStateCountsFallsBackWhenQueueUnpublished(t *testing.T) {
	env := newLimitsTestEnv(t)
	ctx := context.Background()
	env.seed(t, "orphanq", "orphan:x", `{}`, 3)
	// An operator deletes the queue from asynq:queues while an asynq 0.25+
	// producer keeps enqueuing into it (the producer caches "published").
	if err := env.rc.SRem(ctx, "asynq:queues", "orphanq").Err(); err != nil {
		t.Fatalf("SREM asynq:queues: %v", err)
	}
	h := env.router(Options{})

	type countsResp struct {
		Queue  string           `json:"queue"`
		Counts map[string]int64 `json:"counts"`
	}
	var present countsResp
	wPresent := limitsGet(t, h, "/api/tasks/state_counts?queue=orphanq", &present)
	wMissing := limitsGet(t, h, "/api/tasks/state_counts?queue=nosuchq", nil)

	Convey("Given a queue that holds tasks but is absent from asynq:queues", t, func() {
		Convey("When its state counts are read", func() {
			Convey("Then the per-state keys answer instead of a 404", func() {
				So(wPresent.Code, ShouldEqual, 200)
				So(present.Counts["pending"], ShouldEqual, 3)
			})
		})
		Convey("When a queue with no tasks at all is read", func() {
			Convey("Then it is still a 404", func() {
				So(wMissing.Code, ShouldEqual, http.StatusNotFound)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// #47 — legacy list endpoints disclose the clamped page size
// ----------------------------------------------------------------------------

func TestLegacyListReportsClampedPageSize(t *testing.T) {
	env := newLimitsTestEnv(t)
	env.seed(t, "clampq", "clamp:x", `{}`, 25)
	h := env.router(Options{})

	type listResp struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		PageSizeApplied int `json:"page_size_applied"`
	}
	var zero, huge, honest listResp
	wZero := limitsGet(t, h, "/api/queues/clampq/pending_tasks?size=0", &zero)
	wHuge := limitsGet(t, h, "/api/queues/clampq/pending_tasks?size=5000", &huge)
	wHonest := limitsGet(t, h, "/api/queues/clampq/pending_tasks?size=10", &honest)

	Convey("Given 25 pending tasks and a clamped page size", t, func() {
		Convey("When a script asks for size=0 (asynq's \"everything\")", func() {
			Convey("Then the payload reports the size actually applied", func() {
				So(wZero.Code, ShouldEqual, 200)
				So(zero.PageSizeApplied, ShouldEqual, defaultPageSize)
				So(len(zero.Tasks), ShouldEqual, defaultPageSize)
			})
		})
		Convey("When a script asks for size=5000", func() {
			Convey("Then the payload reports the maximum page size applied", func() {
				So(wHuge.Code, ShouldEqual, 200)
				So(huge.PageSizeApplied, ShouldEqual, maxPageSize)
				So(len(huge.Tasks), ShouldEqual, 25)
			})
		})
		Convey("When the requested size needs no clamp", func() {
			Convey("Then the field is absent", func() {
				So(wHonest.Code, ShouldEqual, 200)
				So(honest.PageSizeApplied, ShouldEqual, 0)
				So(len(honest.Tasks), ShouldEqual, 10)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// #48 — tasks re-run through RunTask carry no pending_since
// ----------------------------------------------------------------------------

func TestPendingSinceUnknownCounted(t *testing.T) {
	env := newLimitsTestEnv(t)
	ctx := context.Background()

	// Drive one task through the authentic gap: a worker dequeues it (asynq
	// HDELs pending_since), the handler fails, the task lands in retry, and
	// an operator runs it back to pending — runTaskCmd writes no
	// pending_since. A second task is enqueued afterwards and keeps its
	// record.
	rerun, err := env.client.Enqueue(asynq.NewTask("gap:fails", []byte(`{}`)), asynq.Queue("gapq"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	srv := asynq.NewServer(env.opt, asynq.Config{
		Concurrency: 1,
		Queues:      map[string]int{"gapq": 1},
		RetryDelayFunc: func(n int, e error, task *asynq.Task) time.Duration {
			return 90 * time.Minute
		},
		LogLevel: asynq.FatalLevel,
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc("gap:fails", func(ctx context.Context, task *asynq.Task) error {
		return fmt.Errorf("boom: gateway timeout")
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting failing server: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		qi, qerr := env.insp.GetQueueInfo("gapq")
		if qerr == nil && qi.Retry == 1 {
			break
		}
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("task never reached retry state (info: %+v, err: %v)", qi, qerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	srv.Shutdown()

	if err := env.insp.RunTask("gapq", rerun.ID); err != nil {
		t.Fatalf("running: %v", err)
	}
	kept, err := env.client.Enqueue(asynq.NewTask("gap:fresh", []byte(`{}`)), asynq.Queue("gapq"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	since := pendingSinceTimes(ctx, env.rc, "gapq", []string{kept.ID, rerun.ID})

	h := env.router(Options{})
	type resp struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		Total               int64 `json:"total"`
		PendingSinceUnknown int   `json:"pending_since_unknown"`
	}
	var aged, unknown resp
	wAged := limitsGet(t, h, "/api/tasks?q=state%3Dpending+pending_age%3E1h&queue=gapq", &aged)
	wUnknown := limitsGet(t, h, "/api/tasks?q=state%3Dpending+pending_age%3Dunknown&queue=gapq", &unknown)

	Convey("Given a pending task that reached pending through RunTask", t, func() {
		Convey("When its pending_since is read", func() {
			Convey("Then the enqueued task has a record and the re-run one does not", func() {
				_, keptOK := since[kept.ID]
				_, rerunOK := since[rerun.ID]
				So(keptOK, ShouldBeTrue)
				So(rerunOK, ShouldBeFalse)
			})
		})
		Convey("When a pending_age> scan runs", func() {
			Convey("Then the scan response counts the task it could not evaluate", func() {
				So(wAged.Code, ShouldEqual, 200)
				So(aged.PendingSinceUnknown, ShouldEqual, 1)
			})
		})
		Convey("When pending_age=unknown runs", func() {
			Convey("Then exactly that task is listed", func() {
				So(wUnknown.Code, ShouldEqual, 200)
				So(unknown.Total, ShouldEqual, 1)
				So(len(unknown.Tasks), ShouldEqual, 1)
				So(unknown.Tasks[0].ID, ShouldEqual, rerun.ID)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// #39 — a scan stops when the client goes away
// ----------------------------------------------------------------------------

func TestScanStopsOnCanceledContext(t *testing.T) {
	env := newLimitsTestEnv(t)
	const seeded = 1200 // more than one 1000-task listing page
	env.seed(t, "ctxq", "ctx:x", `{}`, seeded)

	// Cancel from inside the sink, as a disconnecting client would: the scan
	// must not list the next page.
	ctx, cancel := context.WithCancel(context.Background())
	matches := 0
	matched, scanned, truncated, err := scanMatchingTasks(ctx, env.insp, []string{"ctxq"}, "pending", "", nil, 20000, DefaultPayloadFormatter,
		func(st *searchTask) bool {
			matches++
			if matches == 1 {
				cancel()
			}
			return true
		})
	cancel()

	// A context canceled before the scan starts costs no listing at all.
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	_, deadScanned, _, deadErr := scanMatchingTasks(deadCtx, env.insp, []string{"ctxq"}, "pending", "", nil, 20000, DefaultPayloadFormatter,
		func(st *searchTask) bool { return true })

	Convey("Given 1200 pending tasks and a client that disconnects", t, func() {
		Convey("When the context is canceled during the first page", func() {
			Convey("Then the scan stops after that page and reports the cancellation", func() {
				So(err, ShouldEqual, context.Canceled)
				So(truncated, ShouldBeTrue)
				So(scanned, ShouldEqual, searchBatchSize)
				So(matched, ShouldEqual, searchBatchSize)
			})
		})
		Convey("When the context is already canceled", func() {
			Convey("Then no page is listed at all", func() {
				So(deadErr, ShouldEqual, context.Canceled)
				So(deadScanned, ShouldEqual, 0)
			})
		})
	})
}
