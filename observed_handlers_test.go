package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/observe"
)

// ****************************************************************************
// Integration tests for the observe middleware + the /observed endpoint —
// against a real Redis on 127.0.0.1:6379 DB 4. Reserved DBs elsewhere: 5-8
// (aql/search suites), 9 (errsig), 10 (scheduler), 11 (coverage), 12 (jobs),
// 13 (stats handlers), 14 (stats pkg); ONLY DB 4 is flushed here. Skipped
// when Redis does not answer PING.
//
// Attempt records are produced by REAL asynq servers with the middleware
// installed via ServeMux.Use: failing handlers retried by asynq's own retry
// machinery (short RetryDelayFunc + DelayedTaskCheckInterval keep the runs
// fast), a succeeding handler, and a panicking handler proving
// record-then-rethrow. The observe package's Redis-free unit tests live in
// observe/observe_test.go; everything that touches DB 4 is HERE so exactly
// one test binary owns the flush.
//
// Convey discipline (errsig_handlers_test.go precedent): flows that mutate
// queues run imperatively BEFORE the Convey tree; the tree only reads
// captured results.
//
// The JSON field names asserted here are FROZEN frontend contracts
// (ui/src/api.ts ObservedTaskResponse).
// ****************************************************************************

const (
	observedTestRedisAddr = "127.0.0.1:6379"
	observedTestRedisDB   = 4
)

type observedTestEnv struct {
	rc     *redis.Client
	client *asynq.Client
	insp   *asynq.Inspector
}

func newObservedTestEnv(t *testing.T) *observedTestEnv {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: observedTestRedisAddr, DB: observedTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", observedTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	opt := asynq.RedisClientOpt{Addr: observedTestRedisAddr, DB: observedTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.FlushDB(context.Background())
		rc.Close()
	})
	return &observedTestEnv{rc: rc, client: client, insp: insp}
}

// runObservedServer starts a real asynq server over the given queues with the
// observe middleware installed, registers the standard obs:* handlers, waits
// for cond, and shuts down. Retries turn around in ~300ms (RetryDelayFunc
// 200ms + 100ms forwarder interval) so multi-attempt flows stay fast.
func (e *observedTestEnv) runObservedServer(t *testing.T, queues map[string]int, mw func(asynq.Handler) asynq.Handler, cond func() bool) {
	t.Helper()
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: observedTestRedisAddr, DB: observedTestRedisDB},
		asynq.Config{
			Concurrency: 4,
			Queues:      queues,
			RetryDelayFunc: func(n int, err error, task *asynq.Task) time.Duration {
				return 200 * time.Millisecond
			},
			DelayedTaskCheckInterval: 100 * time.Millisecond,
			LogLevel:                 asynq.FatalLevel, // the panic path would otherwise spam ERROR
		},
	)
	h := asynq.NewServeMux()
	h.Use(mw)
	h.HandleFunc("obs:fail", func(ctx context.Context, task *asynq.Task) error {
		time.Sleep(5 * time.Millisecond) // a nonzero, honest duration
		return fmt.Errorf("%s", string(task.Payload()))
	})
	h.HandleFunc("obs:ok", func(ctx context.Context, task *asynq.Task) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	h.HandleFunc("obs:panic", func(ctx context.Context, task *asynq.Task) error {
		panic("boom: " + string(task.Payload()))
	})
	if err := srv.Start(h); err != nil {
		t.Fatalf("starting observed server: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("timed out waiting for observed records")
		}
		time.Sleep(50 * time.Millisecond)
	}
	srv.Shutdown()
}

func (e *observedTestEnv) attempts(t *testing.T, queue, id string) []observe.AttemptRecord {
	t.Helper()
	raw, err := e.rc.LRange(context.Background(), observe.AttemptsKey(observe.DefaultKeyPrefix, queue, id), 0, -1).Result()
	if err != nil {
		t.Fatalf("reading attempts list: %v", err)
	}
	out := make([]observe.AttemptRecord, 0, len(raw))
	for _, entry := range raw {
		var rec observe.AttemptRecord
		if err := json.Unmarshal([]byte(entry), &rec); err != nil {
			t.Fatalf("malformed attempt record %q: %v", entry, err)
		}
		out = append(out, rec)
	}
	return out
}

func (e *observedTestEnv) llen(queue, id string) int64 {
	n, _ := e.rc.LLen(context.Background(), observe.AttemptsKey(observe.DefaultKeyPrefix, queue, id)).Result()
	return n
}

func (e *observedTestEnv) summary(t *testing.T, queue, id string) map[string]string {
	t.Helper()
	m, err := e.rc.HGetAll(context.Background(), observe.SummaryKey(observe.DefaultKeyPrefix, queue, id)).Result()
	if err != nil {
		t.Fatalf("reading summary hash: %v", err)
	}
	return m
}

func (e *observedTestEnv) archivedCount(qname string) int {
	qi, err := e.insp.GetQueueInfo(qname)
	if err != nil {
		return 0
	}
	return qi.Archived
}

func TestObserveMiddlewareRecordsAttempts(t *testing.T) {
	e := newObservedTestEnv(t)

	// ── flow (imperative, before the tree) ───────────────────────────────
	// One task failing through all four attempts (MaxRetry 3 → archived) with
	// an over-cap error message, and one succeeding task.
	longErr := strings.Repeat("x", 620)
	failTask, err := e.client.Enqueue(asynq.NewTask("obs:fail", []byte(longErr)), asynq.Queue("obsq"), asynq.MaxRetry(3))
	if err != nil {
		t.Fatalf("enqueue obs:fail: %v", err)
	}
	okTask, err := e.client.Enqueue(asynq.NewTask("obs:ok", nil), asynq.Queue("obsq"), asynq.MaxRetry(0), asynq.Retention(time.Hour))
	if err != nil {
		t.Fatalf("enqueue obs:ok: %v", err)
	}
	e.runObservedServer(t, map[string]int{"obsq": 1}, observe.Middleware(e.rc), func() bool {
		return e.llen("obsq", failTask.ID) == 4 && e.llen("obsq", okTask.ID) == 1
	})

	failRecs := e.attempts(t, "obsq", failTask.ID)
	okRecs := e.attempts(t, "obsq", okTask.ID)
	sum := e.summary(t, "obsq", failTask.ID)
	attemptsTTL := e.rc.TTL(context.Background(), observe.AttemptsKey(observe.DefaultKeyPrefix, "obsq", failTask.ID)).Val()
	summaryTTL := e.rc.TTL(context.Background(), observe.SummaryKey(observe.DefaultKeyPrefix, "obsq", failTask.ID)).Val()
	host, _ := os.Hostname()
	wantWorker := host + ":" + strconv.Itoa(os.Getpid())

	// ── tree (reads only) ────────────────────────────────────────────────
	Convey("Given a worker with the observe middleware that retried a task to exhaustion", t, func() {
		Convey("Then every attempt asynq ran is recorded, newest first", func() {
			So(failRecs, ShouldHaveLength, 4)
			for i, rec := range failRecs {
				So(rec.N, ShouldEqual, 4-i) // LPUSH order: n = 4,3,2,1
				So(rec.Outcome, ShouldEqual, "error")
				So(rec.Queue, ShouldEqual, "obsq")
				So(rec.TaskID, ShouldEqual, failTask.ID)
				So(rec.Type, ShouldEqual, "obs:fail")
				So(rec.Worker, ShouldEqual, wantWorker)
				So(rec.DurationMs, ShouldBeGreaterThanOrEqualTo, 0)
			}

			Convey("And attempt starts parse as RFC3339 and run oldest to newest", func() {
				var prev time.Time
				for i := len(failRecs) - 1; i >= 0; i-- { // walk chronologically
					ts, err := time.Parse(time.RFC3339Nano, failRecs[i].Start)
					So(err, ShouldBeNil)
					So(ts.Before(prev), ShouldBeFalse)
					prev = ts
				}
			})
			Convey("And the error message is capped at 500 bytes", func() {
				So(len(failRecs[0].Error), ShouldEqual, 500)
				So(failRecs[0].Error, ShouldEqual, longErr[:500])
			})
		})

		Convey("Then the summary hash aggregates the whole run", func() {
			So(sum["total_attempts"], ShouldEqual, "4")
			So(sum["last_outcome"], ShouldEqual, "error")
			So(sum["first_seen"], ShouldEqual, failRecs[3].Start) // oldest attempt's start

			lastMs, err := strconv.ParseInt(sum["last_duration_ms"], 10, 64)
			So(err, ShouldBeNil)
			So(lastMs, ShouldEqual, failRecs[0].DurationMs)

			var totalMs int64
			for _, rec := range failRecs {
				totalMs += rec.DurationMs
			}
			busyMs, err := strconv.ParseInt(sum["total_busy_ms"], 10, 64)
			So(err, ShouldBeNil)
			So(busyMs, ShouldEqual, totalMs)
		})

		Convey("Then both keys carry the default 7d TTL, refreshed per write", func() {
			So(attemptsTTL, ShouldBeGreaterThan, 0)
			So(attemptsTTL, ShouldBeLessThanOrEqualTo, observe.DefaultTTL)
			So(summaryTTL, ShouldBeGreaterThan, 0)
			So(summaryTTL, ShouldBeLessThanOrEqualTo, observe.DefaultTTL)
		})

		Convey("And a succeeding task records a single ok attempt with no error", func() {
			So(okRecs, ShouldHaveLength, 1)
			So(okRecs[0].N, ShouldEqual, 1)
			So(okRecs[0].Outcome, ShouldEqual, "ok")
			So(okRecs[0].Error, ShouldEqual, "")
			So(okRecs[0].DurationMs, ShouldBeGreaterThanOrEqualTo, 5) // the handler sleeps 5ms
		})
	})
}

func TestObserveMiddlewareCapTTLAndPanic(t *testing.T) {
	e := newObservedTestEnv(t)

	// ── flow ─────────────────────────────────────────────────────────────
	// Bounds under override: cap 2, TTL 1h. The failing task runs 4 attempts;
	// the panicking task proves record-then-rethrow (asynq must still treat
	// the attempt as a failure → MaxRetry(0) archives it).
	failTask, err := e.client.Enqueue(asynq.NewTask("obs:fail", []byte("capped run")), asynq.Queue("obscap"), asynq.MaxRetry(3))
	if err != nil {
		t.Fatalf("enqueue obs:fail: %v", err)
	}
	panicTask, err := e.client.Enqueue(asynq.NewTask("obs:panic", []byte("observed panic")), asynq.Queue("obscap"), asynq.MaxRetry(0))
	if err != nil {
		t.Fatalf("enqueue obs:panic: %v", err)
	}
	mw := observe.Middleware(e.rc, observe.WithAttemptCap(2), observe.WithTTL(time.Hour))
	e.runObservedServer(t, map[string]int{"obscap": 1}, mw, func() bool {
		return e.summary(t, "obscap", failTask.ID)["total_attempts"] == "4" &&
			e.llen("obscap", panicTask.ID) == 1 &&
			e.archivedCount("obscap") == 2
	})

	failRecs := e.attempts(t, "obscap", failTask.ID)
	panicRecs := e.attempts(t, "obscap", panicTask.ID)
	sum := e.summary(t, "obscap", failTask.ID)
	attemptsTTL := e.rc.TTL(context.Background(), observe.AttemptsKey(observe.DefaultKeyPrefix, "obscap", failTask.ID)).Val()
	archived := e.archivedCount("obscap")

	// ── tree ─────────────────────────────────────────────────────────────
	Convey("Given the middleware bounded to WithAttemptCap(2) and WithTTL(1h)", t, func() {
		Convey("Then the list is LTRIMmed to the cap, keeping the newest attempts", func() {
			So(failRecs, ShouldHaveLength, 2)
			So(failRecs[0].N, ShouldEqual, 4)
			So(failRecs[1].N, ShouldEqual, 3)

			Convey("And the summary still counts every observed attempt", func() {
				So(sum["total_attempts"], ShouldEqual, "4")
			})
		})

		Convey("Then the configured TTL bounds the keys", func() {
			So(attemptsTTL, ShouldBeGreaterThan, 0)
			So(attemptsTTL, ShouldBeLessThanOrEqualTo, time.Hour)
		})

		Convey("Then a panicking handler is recorded and the panic rethrown", func() {
			So(panicRecs, ShouldHaveLength, 1)
			So(panicRecs[0].Outcome, ShouldEqual, "panic")
			So(panicRecs[0].Error, ShouldContainSubstring, "boom: observed panic")

			Convey("And asynq still saw the panic as a failure (both tasks archived)", func() {
				So(archived, ShouldEqual, 2)
			})
		})
	})
}

func TestObservedEndpoint(t *testing.T) {
	e := newObservedTestEnv(t)

	// ── flow ─────────────────────────────────────────────────────────────
	// Two real attempts (MaxRetry 1 → archived), then read through the REAL
	// router so path wiring is proven, not just the handler func.
	task, err := e.client.Enqueue(asynq.NewTask("obs:fail", []byte("gateway timeout")), asynq.Queue("obsapi"), asynq.MaxRetry(1))
	if err != nil {
		t.Fatalf("enqueue obs:fail: %v", err)
	}
	e.runObservedServer(t, map[string]int{"obsapi": 1}, observe.Middleware(e.rc), func() bool {
		return e.llen("obsapi", task.ID) == 2 && e.archivedCount("obsapi") == 1
	})

	router := muxRouter(Options{}, e.rc, e.insp, nil, nil)
	get := func(url string) (*httptest.ResponseRecorder, observedTaskResponse, map[string]interface{}) {
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var resp observedTaskResponse
		var rawShape map[string]interface{}
		if w.Body.Len() > 0 {
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding %s: %v\nbody: %s", url, err, w.Body.String())
			}
			_ = json.Unmarshal(w.Body.Bytes(), &rawShape)
		}
		return w, resp, rawShape
	}

	wOK, respOK, shapeOK := get("/api/queues/obsapi/tasks/" + task.ID + "/observed")
	wMissing, respMissing, _ := get("/api/queues/obsapi/tasks/no-such-task/observed")

	// ── tree ─────────────────────────────────────────────────────────────
	Convey("Given GET /api/queues/{qname}/tasks/{task_id}/observed", t, func() {
		Convey("When the task has observed records", func() {
			So(wOK.Code, ShouldEqual, http.StatusOK)

			Convey("Then the response carries present=true and both attempts, newest first", func() {
				So(respOK.Present, ShouldBeTrue)
				So(respOK.Attempts, ShouldHaveLength, 2)
				So(respOK.Attempts[0].N, ShouldEqual, 2)
				So(respOK.Attempts[1].N, ShouldEqual, 1)
				for _, a := range respOK.Attempts {
					So(a.Outcome, ShouldEqual, "error")
					So(a.Error, ShouldEqual, "gateway timeout")
					So(a.Start, ShouldNotBeEmpty)
					So(a.Worker, ShouldNotBeEmpty)
					So(a.DurationMs, ShouldBeGreaterThanOrEqualTo, 0)
				}
			})

			Convey("Then the summary block aggregates the run", func() {
				So(respOK.Summary, ShouldNotBeNil)
				So(respOK.Summary.TotalAttempts, ShouldEqual, 2)
				So(respOK.Summary.LastOutcome, ShouldEqual, "error")
				So(respOK.Summary.FirstSeen, ShouldEqual, respOK.Attempts[1].Start)
				So(respOK.Summary.LastDurationMs, ShouldEqual, respOK.Attempts[0].DurationMs)
				So(respOK.Summary.TotalBusyMs, ShouldEqual, respOK.Attempts[0].DurationMs+respOK.Attempts[1].DurationMs)
			})

			Convey("Then the wire shape is the frozen frontend contract", func() {
				So(shapeOK, ShouldContainKey, "present")
				So(shapeOK, ShouldContainKey, "attempts")
				So(shapeOK, ShouldContainKey, "summary")
				attempt := shapeOK["attempts"].([]interface{})[0].(map[string]interface{})
				for _, key := range []string{"n", "start", "duration_ms", "outcome", "error", "worker"} {
					So(attempt, ShouldContainKey, key)
				}
				summary := shapeOK["summary"].(map[string]interface{})
				for _, key := range []string{"first_seen", "total_attempts", "last_duration_ms", "last_outcome", "total_busy_ms"} {
					So(summary, ShouldContainKey, key)
				}
			})
		})

		Convey("When the task has no observed records", func() {
			Convey("Then it answers a clean 404 with present=false and empty attempts", func() {
				So(wMissing.Code, ShouldEqual, http.StatusNotFound)
				So(respMissing.Present, ShouldBeFalse)
				So(respMissing.Attempts, ShouldBeEmpty)
				So(respMissing.Summary, ShouldBeNil)
			})
		})
	})
}
