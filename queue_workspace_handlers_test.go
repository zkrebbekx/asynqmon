package asynqmon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests for the Queue Workspace right-rail endpoints (§3.3(c)):
// GET /api/queues/{qname}/retry_histogram and
// GET /api/queues/{qname}/pending_wait_sample — against a real Redis on
// 127.0.0.1:6379 DB 12 (shared root-test DB; tests in this package run
// sequentially and each env creation flushes it). Fixtures are seeded
// through the real asynq client — retry via a short-lived asynq server
// whose handler always fails — so zset scores and list layouts are
// authentic. Skipped when Redis does not answer PING.
//
// Convey discipline: flows that touch Redis run imperatively BEFORE the
// Convey tree; the tree only reads captured results (jobs_handlers_test.go
// pattern).
// ****************************************************************************

func TestRetryHistogramHandler(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// Drive tasks into retry through the real machinery: a failing handler
	// with RetryDelayFunc = 10 minutes puts next_process_at ≈ now+10m.
	const nRetry = 6
	for i := 0; i < nRetry; i++ {
		if _, err := env.client.Enqueue(
			asynq.NewTask("job:fails", []byte(`{}`)), asynq.Queue("wsretry"),
		); err != nil {
			t.Fatalf("seeding pending: %v", err)
		}
	}
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB},
		asynq.Config{
			Concurrency: 4,
			Queues:      map[string]int{"wsretry": 1},
			RetryDelayFunc: func(n int, e error, task *asynq.Task) time.Duration {
				return 10 * time.Minute
			},
			LogLevel: asynq.FatalLevel,
		},
	)
	mux := asynq.NewServeMux()
	mux.HandleFunc("job:fails", func(ctx context.Context, task *asynq.Task) error {
		return fmt.Errorf("boom")
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting failing server: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		qi, err := env.insp.GetQueueInfo("wsretry")
		if err == nil && qi.Retry == nRetry {
			break
		}
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("tasks never reached retry state (info: %+v, err: %v)", qi, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	srv.Shutdown()

	// 30-minute window: every task (≈ +10m) lands inside the buckets.
	var wide retryHistogramResponse
	wWide := aqlGetJSON(t, h, "/api/queues/wsretry/retry_histogram", &wide)
	// 5-minute window: everything fires beyond the last bucket.
	var narrow retryHistogramResponse
	wNarrow := aqlGetJSON(t, h, "/api/queues/wsretry/retry_histogram?window=300&buckets=5", &narrow)
	// Unknown queue: zeros, never an error (the workspace owns not-found UX).
	var missing retryHistogramResponse
	wMissing := aqlGetJSON(t, h, "/api/queues/no-such-queue/retry_histogram", &missing)

	sum := func(xs []int64) int64 {
		var s int64
		for _, x := range xs {
			s += x
		}
		return s
	}

	Convey("Given 6 tasks driven into retry with next_process_at ≈ now+10m", t, func() {
		Convey("When the default 30m/12-bucket histogram is requested", func() {
			Convey("Then it is exact and the whole mass is inside the buckets", func() {
				So(wWide.Code, ShouldEqual, 200)
				So(wide.Exact, ShouldBeTrue)
				So(wide.Queue, ShouldEqual, "wsretry")
				So(wide.BucketSeconds, ShouldEqual, 150)
				So(len(wide.Buckets), ShouldEqual, 12)
				So(wide.Total, ShouldEqual, nRetry)
				So(wide.PastDue, ShouldEqual, 0)
				So(wide.Beyond, ShouldEqual, 0)
				So(sum(wide.Buckets), ShouldEqual, nRetry)
			})
			Convey("Then the mass sits at the ≈10-minute mark, not spread out", func() {
				// +10m with sub-second slop lands in bucket 3 (450–600s) or
				// 4 (600–750s); asserting the pair keeps the test honest
				// about timing without flaking on the boundary.
				So(wide.Buckets[3]+wide.Buckets[4], ShouldEqual, nRetry)
			})
		})
		Convey("When a 5m window is requested", func() {
			Convey("Then everything is counted beyond the last bucket", func() {
				So(wNarrow.Code, ShouldEqual, 200)
				So(narrow.BucketSeconds, ShouldEqual, 60)
				So(narrow.Total, ShouldEqual, nRetry)
				So(sum(narrow.Buckets), ShouldEqual, 0)
				So(narrow.Beyond, ShouldEqual, nRetry)
			})
		})
		Convey("When an unknown queue is requested", func() {
			Convey("Then the histogram is all zeros, not an error", func() {
				So(wMissing.Code, ShouldEqual, 200)
				So(missing.Total, ShouldEqual, 0)
				So(missing.PastDue, ShouldEqual, 0)
				So(sum(missing.Buckets), ShouldEqual, 0)
			})
		})
	})
}

func TestPendingWaitSampleHandler(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// Seed 12 pending tasks through the real client so pending_since (Unix
	// nanoseconds on the task hash) is written by asynq itself.
	const nPending = 12
	for i := 0; i < nPending; i++ {
		if _, err := env.client.Enqueue(
			asynq.NewTask("job:waits", []byte(`{}`)), asynq.Queue("wswait"),
		); err != nil {
			t.Fatalf("seeding pending: %v", err)
		}
	}
	// And a short 3-element queue to exercise band-overlap dedupe.
	for i := 0; i < 3; i++ {
		if _, err := env.client.Enqueue(
			asynq.NewTask("job:waits", []byte(`{}`)), asynq.Queue("wsshort"),
		); err != nil {
			t.Fatalf("seeding short pending: %v", err)
		}
	}

	var sample pendingWaitSampleResponse
	wSample := aqlGetJSON(t, h, "/api/queues/wswait/pending_wait_sample?probes=5", &sample)
	var short pendingWaitSampleResponse
	wShort := aqlGetJSON(t, h, "/api/queues/wsshort/pending_wait_sample?probes=5", &short)
	var empty pendingWaitSampleResponse
	wEmpty := aqlGetJSON(t, h, "/api/queues/no-such-queue/pending_wait_sample", &empty)

	Convey("Given a 12-task pending LIST sampled with probes=5", t, func() {
		Convey("Then head and tail bands are probed and waits recovered", func() {
			So(wSample.Code, ShouldEqual, 200)
			So(sample.ListLen, ShouldEqual, nPending)
			So(sample.HeadProbes, ShouldEqual, 5)
			So(sample.TailProbes, ShouldEqual, 5)
			So(sample.Sampled, ShouldEqual, 10)
			So(len(sample.WaitsMs), ShouldEqual, 10)
			for _, ms := range sample.WaitsMs {
				So(ms, ShouldBeGreaterThanOrEqualTo, 0)
				So(ms, ShouldBeLessThan, int64(60_000)) // enqueued this test run
			}
		})
		Convey("Then the honesty label is carried verbatim (frozen contract)", func() {
			So(sample.Method, ShouldEqual, "sampled, head/tail-biased")
			So(sample.SampledAt, ShouldNotBeEmpty)
		})
	})

	Convey("Given a 3-task list where the bands overlap", t, func() {
		Convey("Then each element is sampled exactly once", func() {
			So(wShort.Code, ShouldEqual, 200)
			So(short.ListLen, ShouldEqual, 3)
			So(short.HeadProbes+short.TailProbes, ShouldEqual, 3)
			So(short.Sampled, ShouldEqual, 3)
		})
	})

	Convey("Given an unknown queue", t, func() {
		Convey("Then the sample is empty, not an error", func() {
			So(wEmpty.Code, ShouldEqual, 200)
			So(empty.ListLen, ShouldEqual, 0)
			So(empty.Sampled, ShouldEqual, 0)
			So(len(empty.WaitsMs), ShouldEqual, 0)
		})
	})
}
