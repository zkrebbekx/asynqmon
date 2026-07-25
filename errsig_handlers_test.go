package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/errsig"
)

// ****************************************************************************
// Integration tests for the error-signature index (§3.6/§5.7, phase 9) —
// against a real Redis on 127.0.0.1:6379 DB 9. Reserved DBs elsewhere: 10
// (scheduler), 11 (coverage), 12 (jobs), 13 (stats handlers), 14 (stats pkg);
// ONLY DB 9 is flushed here. Skipped when Redis does not answer PING.
//
// Fixtures are seeded through the real asynq machinery: archived tasks are
// produced by a real asynq.Server whose handler fails (MaxRetry(0) exhausts
// retries on the first failure → archived with ErrorMsg set; one task
// additionally proves that wrapping asynq.SkipRetry archives regardless of
// retries remaining). Retry tasks come from a failing handler with a long
// RetryDelayFunc. The single documented exception is the archive-trim
// fixture: reaching the authentic 10k cap needs 10k real failures, so the
// archived zset is hand-filled with dummy members — ZCARD is the exact
// signal the indexer reads for the trim flag, and the dummy members (which
// have no task hashes) also exercise the tail's skip-on-missing-task path.
//
// Convey discipline (jobs_handlers_test.go precedent): flows that mutate
// queues run imperatively BEFORE the Convey tree; the tree only reads
// captured results.
//
// The JSON field names asserted here are FROZEN frontend contracts
// (ui/src/api-errors.ts).
// ****************************************************************************

const (
	errsigTestRedisAddr = "127.0.0.1:6379"
	errsigTestRedisDB   = 9
)

type errsigTestEnv struct {
	rc     *redis.Client
	client *asynq.Client
	insp   *asynq.Inspector
}

func newErrsigTestEnv(t *testing.T) *errsigTestEnv {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: errsigTestRedisAddr, DB: errsigTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", errsigTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	opt := asynq.RedisClientOpt{Addr: errsigTestRedisAddr, DB: errsigTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})
	return &errsigTestEnv{rc: rc, client: client, insp: insp}
}

// newRouter builds the real router against DB 9. Stats engine, SSE broker
// and (crucially) the auto-started indexer are absent — sweeps run
// explicitly via the Indexer under test.
func (e *errsigTestEnv) newRouter() http.Handler {
	return muxRouter(Options{}, e.rc, e.insp, nil, nil)
}

func (e *errsigTestEnv) newIndexer(t *testing.T) *errsig.Indexer {
	t.Helper()
	return errsig.NewIndexer(errsig.Config{
		RedisClient: e.rc,
		Inspector:   e.insp,
		Logf:        t.Logf,
	})
}

// runFailingServer processes the given queues with a handler that fails
// every task using the error message carried in its payload, then waits for
// cond and shuts down. MaxRetry is controlled per task at enqueue time.
func (e *errsigTestEnv) runFailingServer(t *testing.T, queues map[string]int, cond func() bool) {
	t.Helper()
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: errsigTestRedisAddr, DB: errsigTestRedisDB},
		asynq.Config{
			Concurrency: 4,
			Queues:      queues,
			RetryDelayFunc: func(n int, err error, task *asynq.Task) time.Duration {
				return 90 * time.Minute // keep retry tasks parked in the retry zset
			},
			LogLevel: asynq.FatalLevel,
		},
	)
	mux := asynq.NewServeMux()
	mux.HandleFunc("errsig:fail", func(ctx context.Context, task *asynq.Task) error {
		return fmt.Errorf("%s", string(task.Payload()))
	})
	mux.HandleFunc("errsig:skipretry", func(ctx context.Context, task *asynq.Task) error {
		// Verifies §build-note: an error wrapping asynq.SkipRetry archives
		// immediately even with retries remaining.
		return fmt.Errorf("%s: %w", string(task.Payload()), asynq.SkipRetry)
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting failing server: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("timed out waiting for tasks to reach their failure state")
		}
		time.Sleep(100 * time.Millisecond)
	}
	srv.Shutdown()
}

func (e *errsigTestEnv) archivedCount(t *testing.T, qname string) int {
	t.Helper()
	qi, err := e.insp.GetQueueInfo(qname)
	if err != nil {
		return 0
	}
	return qi.Archived
}

func (e *errsigTestEnv) retryCount(t *testing.T, qname string) int {
	t.Helper()
	qi, err := e.insp.GetQueueInfo(qname)
	if err != nil {
		return 0
	}
	return qi.Retry
}

// Wire shapes (mirror of the frozen contract in ui/src/api-errors.ts).
type errSigRowJSON struct {
	Sig               string `json:"sig"`
	Template          string `json:"template"`
	Count             int64  `json:"count"`
	CountIsLowerBound bool   `json:"count_is_lower_bound"`
	ArchivedCount     int64  `json:"archived_count"`
	RetrySampledCount int64  `json:"retry_sampled_count"`
	FirstSeen         string `json:"first_seen"`
	LastSeen          string `json:"last_seen"`
	Trend             string `json:"trend"`
	Queues            []struct {
		Queue        string `json:"queue"`
		Type         string `json:"type"`
		Count        int64  `json:"count"`
		Archived     int64  `json:"archived"`
		RetrySampled int64  `json:"retry_sampled"`
		Sampled      bool   `json:"sampled"`
	} `json:"queues"`
	SampleTaskRefs []struct {
		Queue string `json:"queue"`
		ID    string `json:"id"`
	} `json:"sample_task_refs"`
}

type errSigListJSON struct {
	Signatures      []errSigRowJSON `json:"signatures"`
	TotalSignatures int             `json:"total_signatures"`
	Honesty         struct {
		ArchivedExact bool     `json:"archived_exact"`
		RetrySampled  bool     `json:"retry_sampled"`
		TrimmedQueues []string `json:"trimmed_queues"`
	} `json:"honesty"`
	CounterFailedToday int64  `json:"counter_failed_today"`
	IndexCountTotal    int64  `json:"index_count_total"`
	TailIndexedAt      string `json:"tail_indexed_at"`
	RetrySampledAt     string `json:"retry_sampled_at"`
}

type errSigDetailJSON struct {
	Signature errSigRowJSON `json:"signature"`
	Honesty   struct {
		ArchivedExact bool     `json:"archived_exact"`
		RetrySampled  bool     `json:"retry_sampled"`
		TrimmedQueues []string `json:"trimmed_queues"`
	} `json:"honesty"`
	FailedTodayByQueue []struct {
		Queue  string `json:"queue"`
		Failed int64  `json:"failed"`
	} `json:"failed_today_by_queue"`
	FailedTodayQueuesTotal int64 `json:"failed_today_queues_total"`
	CounterFailedToday     int64 `json:"counter_failed_today"`
	CountOverTimeAvailable bool  `json:"count_over_time_available"`
}

func errsigGetJSON(t *testing.T, h http.Handler, url string, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding %s response: %v\nbody: %s", url, err, w.Body.String())
		}
	}
	return w
}

func findSig(list errSigListJSON, template string) *errSigRowJSON {
	for i := range list.Signatures {
		if list.Signatures[i].Template == template {
			return &list.Signatures[i]
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// Archived tail: real failures → exact index, cursor never reprocesses.
// ----------------------------------------------------------------------------

func TestErrSigArchivedTailIndexAndEndpoints(t *testing.T) {
	env := newErrsigTestEnv(t)
	h := env.newRouter()
	ix := env.newIndexer(t)
	ctx := context.Background()

	// Seed: two distinct failure shapes across two queues, with volatile
	// fragments (host numbers, IPs) that must normalize into ONE template
	// each, plus one SkipRetry task proving forced archive.
	for i := 0; i < 4; i++ {
		msg := fmt.Sprintf("gateway timeout POST https://api-%d.stripe.com/v2/charges", i)
		if _, err := env.client.Enqueue(asynq.NewTask("errsig:fail", []byte(msg)), asynq.Queue("esq"), asynq.MaxRetry(0)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		msg := fmt.Sprintf("connection refused to 10.0.0.%d:5432", i+7)
		if _, err := env.client.Enqueue(asynq.NewTask("errsig:fail", []byte(msg)), asynq.Queue("esq2"), asynq.MaxRetry(0)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if _, err := env.client.Enqueue(asynq.NewTask("errsig:skipretry", []byte("poison payload rejected")), asynq.Queue("esq"), asynq.MaxRetry(25)); err != nil {
		t.Fatalf("enqueue skipretry: %v", err)
	}
	env.runFailingServer(t, map[string]int{"esq": 1, "esq2": 1}, func() bool {
		return env.archivedCount(t, "esq") == 5 && env.archivedCount(t, "esq2") == 2
	})

	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("tail sweep: %v", err)
	}
	var first errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &first)

	// Second sweep with no new archives: the (score,id) cursor must not
	// reprocess anything.
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("second tail sweep: %v", err)
	}
	var second errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &second)

	// Two more real failures land AFTER the cursor: only the delta indexes.
	for i := 0; i < 2; i++ {
		msg := fmt.Sprintf("gateway timeout POST https://api-%d.stripe.com/v2/charges", 40+i)
		if _, err := env.client.Enqueue(asynq.NewTask("errsig:fail", []byte(msg)), asynq.Queue("esq"), asynq.MaxRetry(0)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	env.runFailingServer(t, map[string]int{"esq": 1}, func() bool {
		return env.archivedCount(t, "esq") == 7
	})
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("third tail sweep: %v", err)
	}
	var third errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures?sort=count", &third)

	gwTemplate := "gateway timeout POST https://~HOST~/v2/charges"
	connTemplate := "connection refused to ~HOST~"

	var detail errSigDetailJSON
	var detailCode int
	if s := findSig(third, gwTemplate); s != nil {
		detailCode = errsigGetJSON(t, h, "/api/errors/signatures/"+s.Sig, &detail).Code
	}
	notFound := errsigGetJSON(t, h, "/api/errors/signatures/ffffffffffffffffffffffffffffffff", nil)
	badSort := errsigGetJSON(t, h, "/api/errors/signatures?sort=bogus", nil)

	Convey("Given tasks archived through real asynq failures", t, func() {
		Convey("When the archived tail sweeps", func() {
			Convey("Then volatile hosts/IPs normalize into one signature each", func() {
				So(len(first.Signatures), ShouldEqual, 3)
				So(findSig(first, gwTemplate), ShouldNotBeNil)
				So(findSig(first, connTemplate), ShouldNotBeNil)
			})
			Convey("Then the gateway signature is exact over its 4 archived tasks", func() {
				s := findSig(first, gwTemplate)
				So(s.Count, ShouldEqual, 4)
				So(s.ArchivedCount, ShouldEqual, 4)
				So(s.RetrySampledCount, ShouldEqual, 0)
				So(s.CountIsLowerBound, ShouldBeFalse)
				So(s.Trend, ShouldEqual, "new")
				So(s.FirstSeen, ShouldNotBeEmpty)
				So(s.LastSeen, ShouldNotBeEmpty)
			})
			Convey("Then the blast-radius matrix carries queue and type", func() {
				s := findSig(first, gwTemplate)
				So(len(s.Queues), ShouldEqual, 1)
				So(s.Queues[0].Queue, ShouldEqual, "esq")
				So(s.Queues[0].Type, ShouldEqual, "errsig:fail")
				So(s.Queues[0].Archived, ShouldEqual, 4)
				So(s.Queues[0].Sampled, ShouldBeFalse)
			})
			Convey("Then sample task refs point at real archived tasks", func() {
				s := findSig(first, gwTemplate)
				So(len(s.SampleTaskRefs), ShouldBeGreaterThanOrEqualTo, 1)
				So(s.SampleTaskRefs[0].Queue, ShouldEqual, "esq")
				So(s.SampleTaskRefs[0].ID, ShouldNotBeEmpty)
			})
			Convey("Then a SkipRetry-wrapped error archived despite retries remaining", func() {
				s := findSig(first, "poison payload rejected: skip retry for the task")
				So(s, ShouldNotBeNil)
				So(s.ArchivedCount, ShouldEqual, 1)
			})
			Convey("Then the counter cross-check carries today's failed sum", func() {
				So(first.CounterFailedToday, ShouldEqual, 7)
				So(first.IndexCountTotal, ShouldEqual, 7)
				So(first.TailIndexedAt, ShouldNotBeEmpty)
			})
			Convey("Then the honesty ribbon states its sources", func() {
				So(first.Honesty.ArchivedExact, ShouldBeTrue)
				So(first.Honesty.RetrySampled, ShouldBeTrue)
				So(len(first.Honesty.TrimmedQueues), ShouldEqual, 0)
			})
		})
		Convey("When the tail sweeps again with no new archives", func() {
			Convey("Then the cursor reprocesses nothing — counts are unchanged", func() {
				So(findSig(second, gwTemplate).Count, ShouldEqual, 4)
				So(findSig(second, connTemplate).Count, ShouldEqual, 2)
				So(second.IndexCountTotal, ShouldEqual, first.IndexCountTotal)
			})
		})
		Convey("When two more failures land after the cursor", func() {
			Convey("Then exactly the delta is indexed", func() {
				So(findSig(third, gwTemplate).Count, ShouldEqual, 6)
				So(findSig(third, connTemplate).Count, ShouldEqual, 2)
			})
			Convey("Then sort=count puts the biggest signature first", func() {
				So(third.Signatures[0].Template, ShouldEqual, gwTemplate)
			})
		})
		Convey("When the detail endpoint is asked for the gateway signature", func() {
			Convey("Then it answers with the per-queue counter cross-check", func() {
				So(detailCode, ShouldEqual, http.StatusOK)
				So(detail.Signature.Template, ShouldEqual, gwTemplate)
				So(detail.Signature.Count, ShouldEqual, 6)
				So(len(detail.FailedTodayByQueue), ShouldEqual, 1)
				So(detail.FailedTodayByQueue[0].Queue, ShouldEqual, "esq")
				So(detail.FailedTodayByQueue[0].Failed, ShouldEqual, 7) // all esq failures, incl. the poison task
				So(detail.CountOverTimeAvailable, ShouldBeFalse)
			})
		})
		Convey("When an unknown signature or a bad sort is requested", func() {
			Convey("Then the endpoint rejects with 404 / 400", func() {
				So(notFound.Code, ShouldEqual, http.StatusNotFound)
				So(badSort.Code, ShouldEqual, http.StatusBadRequest)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Retry sampler: bounded resample, flagged sampled, replaced wholesale.
// ----------------------------------------------------------------------------

func TestErrSigRetrySamplerFlags(t *testing.T) {
	env := newErrsigTestEnv(t)
	h := env.newRouter()
	ix := env.newIndexer(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := env.client.Enqueue(asynq.NewTask("errsig:fail", []byte("context deadline exceeded after 30s")), asynq.Queue("rsq"), asynq.MaxRetry(5)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	env.runFailingServer(t, map[string]int{"rsq": 1}, func() bool {
		return env.retryCount(t, "rsq") == 3
	})

	if err := ix.SampleRetryNow(ctx); err != nil {
		t.Fatalf("retry sample: %v", err)
	}
	var first errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &first)

	// A second resample of the SAME parked tasks must not double-count:
	// contributions are replaced wholesale, never accumulated.
	if err := ix.SampleRetryNow(ctx); err != nil {
		t.Fatalf("second retry sample: %v", err)
	}
	var second errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &second)

	// Drain the retry set (the operator fixed the bug): the sampled-only
	// signature must disappear rather than linger as a ghost.
	if _, err := env.insp.DeleteAllRetryTasks("rsq"); err != nil {
		t.Fatalf("draining retry: %v", err)
	}
	if err := ix.SampleRetryNow(ctx); err != nil {
		t.Fatalf("post-drain retry sample: %v", err)
	}
	var drained errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &drained)

	template := "context deadline exceeded after ~N~s"

	Convey("Given tasks parked in retry by a failing worker", t, func() {
		Convey("When the retry sampler runs", func() {
			Convey("Then the signature exists with sampled contributions only", func() {
				s := findSig(first, template)
				So(s, ShouldNotBeNil)
				So(s.Count, ShouldEqual, 3)
				So(s.ArchivedCount, ShouldEqual, 0)
				So(s.RetrySampledCount, ShouldEqual, 3)
			})
			Convey("Then the queue cell is flagged sampled", func() {
				s := findSig(first, template)
				So(len(s.Queues), ShouldEqual, 1)
				So(s.Queues[0].Sampled, ShouldBeTrue)
				So(s.Queues[0].RetrySampled, ShouldEqual, 3)
			})
			Convey("Then the sample stamp is set", func() {
				So(first.RetrySampledAt, ShouldNotBeEmpty)
			})
		})
		Convey("When the same parked tasks are resampled", func() {
			Convey("Then counts do not accumulate — a resample replaces, never adds", func() {
				So(findSig(second, template).Count, ShouldEqual, 3)
			})
		})
		Convey("When the retry set drains", func() {
			Convey("Then the sampled-only signature is removed entirely", func() {
				So(findSig(drained, template), ShouldBeNil)
				So(drained.TotalSignatures, ShouldEqual, 0)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Archive trim: ZCARD at the 10k cap flags every sourced count lower-bound.
// ----------------------------------------------------------------------------

func TestErrSigTrimLowerBound(t *testing.T) {
	env := newErrsigTestEnv(t)
	h := env.newRouter()
	ix := env.newIndexer(t)
	ctx := context.Background()

	if _, err := env.client.Enqueue(asynq.NewTask("errsig:fail", []byte("disk full writing block")), asynq.Queue("trimq"), asynq.MaxRetry(0)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	env.runFailingServer(t, map[string]int{"trimq": 1}, func() bool {
		return env.archivedCount(t, "trimq") == 1
	})
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("tail sweep: %v", err)
	}
	var before errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &before)

	// Hand-fill the archived zset up to the 10k trim cap. Documented
	// authenticity exception: producing the cap authentically needs 10,000
	// real failures plus asynq's own trim; ZCARD == 10000 is the exact
	// signal the indexer reads. The dummy members carry no task hashes, so
	// they also exercise the tail's skip-on-missing-task path (each is
	// examined once, advances the cursor, and indexes nothing).
	scoreBase := float64(time.Now().Unix())
	const fillBatch = 500
	for start := 0; start < 9999; start += fillBatch {
		zs := make([]redis.Z, 0, fillBatch)
		for i := start; i < start+fillBatch && i < 9999; i++ {
			zs = append(zs, redis.Z{Score: scoreBase, Member: fmt.Sprintf("trim-dummy-%05d", i)})
		}
		if err := env.rc.ZAdd(ctx, "asynq:{trimq}:archived", zs...).Err(); err != nil {
			t.Fatalf("hand-filling archived zset: %v", err)
		}
	}
	// Two sweeps: the per-queue tail budget (1000/sweep) means one sweep
	// examines only part of the dummy range; the trim flag must be visible
	// after the FIRST sweep regardless (ZCARD is read before tailing).
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("post-fill sweep: %v", err)
	}
	var after errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &after)
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("second post-fill sweep: %v", err)
	}
	var again errSigListJSON
	errsigGetJSON(t, h, "/api/errors/signatures", &again)

	template := "disk full writing block"

	Convey("Given a queue whose archive sits at the 10k trim cap", t, func() {
		Convey("When the tail sweeps before the fill", func() {
			Convey("Then the signature is not a lower bound", func() {
				s := findSig(before, template)
				So(s, ShouldNotBeNil)
				So(s.CountIsLowerBound, ShouldBeFalse)
				So(len(before.Honesty.TrimmedQueues), ShouldEqual, 0)
			})
		})
		Convey("When the archive reaches the cap", func() {
			Convey("Then the honesty ribbon lists the trimmed queue", func() {
				So(after.Honesty.TrimmedQueues, ShouldResemble, []string{"trimq"})
			})
			Convey("Then every count sourced from it is flagged a lower bound", func() {
				s := findSig(after, template)
				So(s, ShouldNotBeNil)
				So(s.CountIsLowerBound, ShouldBeTrue)
			})
			Convey("Then dummy members index nothing and never wedge the cursor", func() {
				So(findSig(after, template).Count, ShouldEqual, 1)
				So(findSig(again, template).Count, ShouldEqual, 1)
				So(again.TotalSignatures, ShouldEqual, 1)
			})
		})
	})
}
