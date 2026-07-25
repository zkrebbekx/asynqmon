package asynqmon

import (
	"context"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Integration tests for the Workers screen endpoints against a real Redis on
// 127.0.0.1:6379 DB 11. (DB ownership: 13 = fleet handler tests, 14 = stats
// package tests; DB 11 belongs to the Workers/coverage tests — only DB 11 is
// flushed here.) Fixtures run REAL asynq.Server instances so ServerInfo
// (priority maps, StrictPriority, ActiveWorkers with deadlines) is authentic.
// Skipped when Redis does not answer PING.
//
// Note on PUBSUB NUMSUB: pub/sub channels are instance-global, not per-DB,
// so listener counts assert deltas, never absolute values.
// ****************************************************************************

const (
	coverageTestRedisAddr = "127.0.0.1:6379"
	coverageTestRedisDB   = 11
)

// coverageRedis pings (or skips), flushes DB 11 only, and returns the shared
// clients used by the fixtures.
func coverageRedis(t *testing.T) (*redis.Client, *asynq.Inspector, asynq.RedisClientOpt) {
	t.Helper()
	ctx := context.Background()

	rc := redis.NewClient(&redis.Options{Addr: coverageTestRedisAddr, DB: coverageTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", coverageTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing test db %d: %v", coverageTestRedisDB, err)
	}
	t.Cleanup(func() { rc.Close() })

	opt := asynq.RedisClientOpt{Addr: coverageTestRedisAddr, DB: coverageTestRedisDB}
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { insp.Close() })
	return rc, insp, opt
}

// startServer runs a real asynq.Server with the given queue map and returns
// after registering its shutdown. The handler blocks until test cleanup so
// picked-up tasks stay visibly active (cleanup unblocks it before Shutdown
// waits on workers).
func startServer(t *testing.T, opt asynq.RedisClientOpt, concurrency int, strict bool, queues map[string]int) {
	t.Helper()
	srv := asynq.NewServer(opt, asynq.Config{
		Concurrency:    concurrency,
		Queues:         queues,
		StrictPriority: strict,
		LogLevel:       asynq.FatalLevel,
	})
	block := make(chan struct{})
	h := asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	})
	if err := srv.Start(h); err != nil {
		t.Fatalf("starting asynq server: %v", err)
	}
	// LIFO cleanup: unblock handlers first, then Shutdown (which waits for
	// workers), so shutdown never eats the 8s worker-drain timeout.
	t.Cleanup(srv.Shutdown)
	t.Cleanup(func() { close(block) })
}

// waitFor polls cond every 100ms until it returns true or the deadline
// passes; heartbeat-driven state (server registry, active workers) has no
// synchronous read.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// newCoverageFixture seeds DB 11 and runs two real asynq servers:
//
//	server A: strict,   {high:3, low:1}, concurrency 4
//	server B: fair,     {plain:2, confonly:1}, concurrency 2
//
// Queues high/low/plain are enqueued into and then PAUSED before the servers
// start, so their pending depths are frozen and deterministic:
// unwatched(3 pending, no consumer), high(2), low(1), plain(1);
// "confonly" exists only in server B's config. Returns a swept stats engine.
func newCoverageFixture(t *testing.T) (*asynq.Inspector, *stats.Engine) {
	t.Helper()
	ctx := context.Background()
	rc, insp, opt := coverageRedis(t)

	client := asynq.NewClient(opt)
	t.Cleanup(func() { client.Close() })
	seed := map[string]int{"unwatched": 3, "high": 2, "low": 1, "plain": 1}
	for qname, n := range seed {
		for i := 0; i < n; i++ {
			if _, err := client.Enqueue(asynq.NewTask("seed:task", nil), asynq.Queue(qname)); err != nil {
				t.Fatalf("enqueue into %s: %v", qname, err)
			}
		}
	}
	// Freeze consumption BEFORE any server starts: depths stay exactly as
	// seeded, and the no-op handler is never invoked.
	for _, qname := range []string{"high", "low", "plain"} {
		if err := insp.PauseQueue(qname); err != nil {
			t.Fatalf("pause %s: %v", qname, err)
		}
	}

	startServer(t, opt, 4, true, map[string]int{"high": 3, "low": 1})
	startServer(t, opt, 2, false, map[string]int{"plain": 2, "confonly": 1})
	waitFor(t, 10*time.Second, "both servers to heartbeat", func() bool {
		srvs, err := insp.Servers()
		return err == nil && len(srvs) == 2
	})

	engine := stats.NewEngine(stats.Config{
		RedisClient: rc,
		Inspector:   insp,
		Logf:        func(string, ...interface{}) {},
	})
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return insp, engine
}

func pendingOf(r *coverageRow) int64 {
	if r == nil || r.Pending == nil {
		return -1
	}
	return *r.Pending
}

func TestCoverageHandler(t *testing.T) {
	insp, engine := newCoverageFixture(t)
	handler := newCoverageHandlerFunc(insp, engine)

	Convey("Given two live asynq servers and a swept stats cache", t, func() {
		Convey("When GET /api/coverage is served", func() {
			var resp coverageResponse
			w := getJSON(t, handler, "/api/coverage", &resp)

			Convey("Then it returns the union of registered and config-only queues", func() {
				So(w.Code, ShouldEqual, 200)
				So(resp.UpdatedAt, ShouldNotBeEmpty)
				So(len(resp.Rows), ShouldEqual, 5) // unwatched, high, low, plain, confonly
			})

			Convey("Then the consumer-less pending queue is UNCOVERED and sorted first", func() {
				So(resp.Rows[0].Queue, ShouldEqual, "unwatched")
				So(resp.Rows[0].Status, ShouldEqual, coverageStatusUncovered)
				So(pendingOf(resp.Rows[0]), ShouldEqual, 3)
				So(resp.Rows[0].Consumers, ShouldBeEmpty)
				So(resp.Rows[0].TotalConcurrency, ShouldEqual, 0)
			})

			Convey("Then the strict server's lowest-weight queue is STARVED (high is non-empty)", func() {
				r := rowByQueue(resp.Rows, "low")
				So(r, ShouldNotBeNil)
				So(r.Status, ShouldEqual, coverageStatusStarved)
				So(pendingOf(r), ShouldEqual, 1)
				So(len(r.Consumers), ShouldEqual, 1)
				So(r.Consumers[0].Weight, ShouldEqual, 1)
				So(r.Consumers[0].StrictPriority, ShouldBeTrue)
				So(r.Consumers[0].Host, ShouldNotBeEmpty)
				So(r.Consumers[0].PID, ShouldBeGreaterThan, 0)
			})

			Convey("Then the top-weight and fairly-consumed queues are ok with real consumer data", func() {
				high := rowByQueue(resp.Rows, "high")
				So(high.Status, ShouldEqual, coverageStatusOK)
				So(pendingOf(high), ShouldEqual, 2)
				So(high.TotalConcurrency, ShouldEqual, 4)

				plain := rowByQueue(resp.Rows, "plain")
				So(plain.Status, ShouldEqual, coverageStatusOK)
				So(plain.Consumers[0].StrictPriority, ShouldBeFalse)
				So(plain.TotalConcurrency, ShouldEqual, 2)
			})

			Convey("Then a config-only queue appears with pending omitted (no cache row)", func() {
				r := rowByQueue(resp.Rows, "confonly")
				So(r, ShouldNotBeNil)
				So(r.Pending, ShouldBeNil)
				So(r.Status, ShouldEqual, coverageStatusOK)
			})
		})

		Convey("When the stats engine is disabled (nil)", func() {
			var resp coverageResponse
			w := getJSON(t, newCoverageHandlerFunc(insp, nil), "/api/coverage", &resp)

			Convey("Then coverage still answers (200) with pending omitted everywhere", func() {
				So(w.Code, ShouldEqual, 200)
				So(len(resp.Rows), ShouldEqual, 5)
				for _, r := range resp.Rows {
					So(r.Pending, ShouldBeNil)
				}
			})
			Convey("Then no uncovered/starved claim is made without depth evidence", func() {
				So(rowByQueue(resp.Rows, "unwatched").Status, ShouldEqual, coverageStatusOK)
				So(rowByQueue(resp.Rows, "low").Status, ShouldEqual, coverageStatusOK)
			})
		})
	})
}

func TestCancelListenersHandler(t *testing.T) {
	ctx := context.Background()
	rc, _, _ := coverageRedis(t)
	handler := newCancelListenersHandlerFunc(rc)

	listeners := func() int64 {
		var resp cancelListenersResponse
		w := getJSON(t, handler, "/api/cancel-listeners", &resp)
		if w.Code != 200 {
			t.Fatalf("cancel-listeners returned %d: %s", w.Code, w.Body.String())
		}
		return resp.Listeners
	}

	Convey("Given a single-node Redis", t, func() {
		Convey("When GET /api/cancel-listeners is served", func() {
			var resp cancelListenersResponse
			w := getJSON(t, handler, "/api/cancel-listeners", &resp)

			Convey("Then the count is exact — one node, not approximate", func() {
				So(w.Code, ShouldEqual, 200)
				So(resp.Approximate, ShouldBeFalse)
				So(resp.Nodes, ShouldEqual, 1)
				So(resp.Listeners, ShouldBeGreaterThanOrEqualTo, 0)
			})
		})

		Convey("When two more subscribers join asynq's cancelation channel", func() {
			before := listeners()
			sub1 := rc.Subscribe(ctx, asynqCancelChannel)
			sub2 := rc.Subscribe(ctx, asynqCancelChannel)
			defer sub1.Close()
			defer sub2.Close()
			// Receive waits for the subscription confirmations so NUMSUB
			// counts them.
			if _, err := sub1.Receive(ctx); err != nil {
				t.Fatalf("subscribe 1: %v", err)
			}
			if _, err := sub2.Receive(ctx); err != nil {
				t.Fatalf("subscribe 2: %v", err)
			}

			Convey("Then the listener count rises by (at least) two", func() {
				// ≥, not ==: pub/sub is instance-global and parallel work on
				// other DBs may add its own subscribers.
				So(listeners(), ShouldBeGreaterThanOrEqualTo, before+2)
			})
		})
	})
}

func TestListServersHandlerWorkerDeadline(t *testing.T) {
	_, insp, opt := coverageRedis(t)

	client := asynq.NewClient(opt)
	t.Cleanup(func() { client.Close() })
	if _, err := client.Enqueue(asynq.NewTask("busy:task", nil),
		asynq.Queue("busy"), asynq.Timeout(30*time.Second)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Unpaused queue + blocking handler: the task is picked up and stays
	// visibly active with a real start time and deadline in the heartbeat.
	startServer(t, opt, 1, false, map[string]int{"busy": 1})
	waitFor(t, 15*time.Second, "an active worker in the heartbeat", func() bool {
		srvs, err := insp.Servers()
		return err == nil && len(srvs) == 1 && len(srvs[0].ActiveWorkers) == 1
	})

	handler := newListServersHandlerFunc(insp, DefaultPayloadFormatter)

	Convey("Given a real server processing a task with a 30s timeout", t, func() {
		Convey("When GET /api/servers is served", func() {
			var resp listServersResponse
			w := getJSON(t, handler, "/api/servers", &resp)

			Convey("Then the active worker carries start_time AND deadline for the budget bar (§3.7)", func() {
				So(w.Code, ShouldEqual, 200)
				So(len(resp.Servers), ShouldEqual, 1)
				So(len(resp.Servers[0].ActiveWorkers), ShouldEqual, 1)
				worker := resp.Servers[0].ActiveWorkers[0]
				So(worker.Started, ShouldNotBeEmpty)
				So(worker.Deadline, ShouldNotBeEmpty)

				started, err := time.Parse(time.RFC3339, worker.Started)
				So(err, ShouldBeNil)
				deadline, err := time.Parse(time.RFC3339, worker.Deadline)
				So(err, ShouldBeNil)
				Convey("And the budget (deadline − start) is the enqueued 30s timeout", func() {
					So(deadline.Sub(started), ShouldEqual, 30*time.Second)
				})
			})
		})
	})
}
