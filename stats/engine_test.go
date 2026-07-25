package stats

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests against a real Redis on 127.0.0.1:6379.
//
// DB 14 is reserved for this package (the root package's handler tests use
// DB 13, so parallel per-package `go test ./...` runs cannot flush each
// other's fixtures). Tests are skipped when Redis does not answer PING.
//
// Fixtures are seeded through the real asynq client/inspector/server — never
// hand-built keys — so the key layouts the sweeper reads are exactly what
// asynq v0.24.1 produces. The single documented exception is the expired
// lease entry in TestSweepSeededFleet: creating one authentically requires
// killing a worker mid-task and waiting out the 30s lease plus grace.
// ****************************************************************************

const (
	testRedisAddr = "127.0.0.1:6379"
	testRedisDB   = 14
)

// testRedis returns a flushed DB-14 client, skipping the test when Redis is
// unavailable.
func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: testRedisAddr, DB: testRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", testRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

func testAsynqOpt() asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: testRedisAddr, DB: testRedisDB}
}

func testClientInspector(t *testing.T) (*asynq.Client, *asynq.Inspector) {
	t.Helper()
	client := asynq.NewClient(testAsynqOpt())
	insp := asynq.NewInspector(testAsynqOpt())
	t.Cleanup(func() {
		client.Close()
		insp.Close()
	})
	return client, insp
}

// silentLogf keeps engine lifecycle chatter out of test output and prevents
// any logging from racing test completion.
func silentLogf(string, ...interface{}) {}

// eventually polls cond until it returns true or the timeout expires.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// waitFor is eventually with a fatal timeout, for test orchestration outside
// Convey blocks.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	if !eventually(timeout, cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestMirroredKeyFormats is the tripwire for asynq-internals drift: it seeds
// Redis exclusively through the real asynq client/inspector and asserts our
// mirrored key formats (keys.go) point at the structures asynq actually
// wrote. If an asynq upgrade moves a key, this fails before any sweep test
// gets a chance to mislead.
func TestMirroredKeyFormats(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	pending, err := client.Enqueue(asynq.NewTask("kf:one", []byte(`{"n":1}`)), asynq.Queue("kq"))
	if err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if _, err := client.Enqueue(asynq.NewTask("kf:later", nil), asynq.Queue("kq"), asynq.ProcessIn(time.Hour)); err != nil {
		t.Fatalf("enqueue scheduled: %v", err)
	}
	if _, err := client.Enqueue(asynq.NewTask("kf:grouped", nil), asynq.Queue("kq"), asynq.Group("kg")); err != nil {
		t.Fatalf("enqueue grouped: %v", err)
	}
	toArchive, err := client.Enqueue(asynq.NewTask("kf:two", nil), asynq.Queue("kq"))
	if err != nil {
		t.Fatalf("enqueue to-archive: %v", err)
	}
	if err := insp.ArchiveTask("kq", toArchive.ID); err != nil {
		t.Fatalf("archive task: %v", err)
	}
	if err := insp.PauseQueue("kq"); err != nil {
		t.Fatalf("pause queue: %v", err)
	}

	Convey("Given a queue seeded through the real asynq client", t, func() {
		Convey("Then the queue set key matches", func() {
			So(rc.SIsMember(ctx, allQueuesKey, "kq").Val(), ShouldBeTrue)
		})
		Convey("Then pending is a LIST at the mirrored key", func() {
			So(rc.Type(ctx, pendingKey("kq")).Val(), ShouldEqual, "list")
			So(rc.LLen(ctx, pendingKey("kq")).Val(), ShouldEqual, 1)
		})
		Convey("Then the task hash holds pending_since as unix nanos", func() {
			v, err := rc.HGet(ctx, taskKey("kq", pending.ID), "pending_since").Result()
			So(err, ShouldBeNil)
			nanos, err := strconv.ParseInt(v, 10, 64)
			So(err, ShouldBeNil)
			So(nanos, ShouldBeGreaterThan, 0)
		})
		Convey("Then scheduled and archived are ZSETs at the mirrored keys", func() {
			So(rc.Type(ctx, scheduledKey("kq")).Val(), ShouldEqual, "zset")
			So(rc.ZCard(ctx, scheduledKey("kq")).Val(), ShouldEqual, 1)
			So(rc.Type(ctx, archivedKey("kq")).Val(), ShouldEqual, "zset")
			So(rc.ZCard(ctx, archivedKey("kq")).Val(), ShouldEqual, 1)
		})
		Convey("Then the paused marker holds the pause time as unix seconds", func() {
			v, err := rc.Get(ctx, pausedKey("kq")).Result()
			So(err, ShouldBeNil)
			sec, err := strconv.ParseInt(v, 10, 64)
			So(err, ShouldBeNil)
			So(sec, ShouldBeGreaterThan, 0)
		})
		Convey("Then the groups set holds the group name", func() {
			So(rc.SCard(ctx, allGroupsKey("kq")).Val(), ShouldEqual, 1)
		})
	})
}

// TestSweepSeededFleet covers the full sweep + cache write + local/cache read
// paths over an asynq-client-seeded fleet with no servers running.
func TestSweepSeededFleet(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	// q1: 3 pending, 1 scheduled far future, 1 scheduled that will fall past
	// due (no asynq server is running, so no forwarder moves it — exactly the
	// stall the past_due signal exists to expose).
	for i := 0; i < 3; i++ {
		if _, err := client.Enqueue(asynq.NewTask("t:pending", nil), asynq.Queue("q1")); err != nil {
			t.Fatalf("enqueue q1 pending: %v", err)
		}
	}
	if _, err := client.Enqueue(asynq.NewTask("t:later", nil), asynq.Queue("q1"), asynq.ProcessIn(time.Hour)); err != nil {
		t.Fatalf("enqueue q1 scheduled: %v", err)
	}
	if _, err := client.Enqueue(asynq.NewTask("t:stalled", nil), asynq.Queue("q1"), asynq.ProcessIn(300*time.Millisecond)); err != nil {
		t.Fatalf("enqueue q1 soon-scheduled: %v", err)
	}

	// q2: 2 pending (one then archived), 1 aggregating group task, paused.
	if _, err := client.Enqueue(asynq.NewTask("t:keep", nil), asynq.Queue("q2")); err != nil {
		t.Fatalf("enqueue q2 pending: %v", err)
	}
	toArchive, err := client.Enqueue(asynq.NewTask("t:dead", nil), asynq.Queue("q2"))
	if err != nil {
		t.Fatalf("enqueue q2 to-archive: %v", err)
	}
	if err := insp.ArchiveTask("q2", toArchive.ID); err != nil {
		t.Fatalf("archive q2 task: %v", err)
	}
	if _, err := client.Enqueue(asynq.NewTask("t:grouped", nil), asynq.Queue("q2"), asynq.Group("g1")); err != nil {
		t.Fatalf("enqueue q2 grouped: %v", err)
	}
	if err := insp.PauseQueue("q2"); err != nil {
		t.Fatalf("pause q2: %v", err)
	}

	// Hand-built exception (see file header): one expired and one live lease
	// entry. An authentic expired lease needs a worker killed mid-task plus a
	// 30s+grace wait; the score semantics (lease-expiry unix seconds) are
	// verified against asynq's dequeue script, and the layout tripwire above
	// covers key placement.
	now := time.Now()
	if err := rc.ZAdd(ctx, leaseKey("q1"),
		redis.Z{Score: float64(now.Add(-2 * time.Minute).Unix()), Member: "orphaned-task-id"},
		redis.Z{Score: float64(now.Add(time.Minute).Unix()), Member: "healthy-task-id"},
	).Err(); err != nil {
		t.Fatalf("seed lease entries: %v", err)
	}

	// Let the soon-scheduled task's process-at time pass so it is past due.
	time.Sleep(600 * time.Millisecond)

	engine := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
	sweepErr := engine.SweepNow(ctx)

	Convey("Given a seeded two-queue fleet and one completed sweep", t, func() {
		So(sweepErr, ShouldBeNil)
		res, err := engine.Read(ctx)
		So(err, ShouldBeNil)
		So(res.Source, ShouldEqual, SourceLocal)
		So(res.Queues, ShouldHaveLength, 2)
		byName := map[string]*QueueSnapshot{}
		for _, s := range res.Queues {
			byName[s.Queue] = s
		}

		Convey("Then q1's snapshot reflects every seeded state", func() {
			q1 := byName["q1"]
			So(q1, ShouldNotBeNil)
			So(q1.Pending, ShouldEqual, 3)
			So(q1.Active, ShouldEqual, 0)
			So(q1.Scheduled, ShouldEqual, 2)
			So(q1.PastDueScheduled, ShouldEqual, 1)
			So(q1.Retry, ShouldEqual, 0)
			So(q1.NextRetryAt.IsZero(), ShouldBeTrue)
			So(q1.OrphanCandidates, ShouldEqual, 1) // expired lease only, not the live one
			So(q1.Paused, ShouldBeFalse)
			So(q1.Groups, ShouldEqual, 0)
			So(q1.OldestPendingSince.IsZero(), ShouldBeFalse)
			So(q1.OldestPendingAge(time.Now()), ShouldBeGreaterThan, 0)
			So(q1.Consumers, ShouldEqual, 0) // no servers running
			So(q1.RefreshedAt.IsZero(), ShouldBeFalse)
		})

		Convey("Then q2's snapshot reflects pause, archive and grouping", func() {
			q2 := byName["q2"]
			So(q2, ShouldNotBeNil)
			So(q2.Pending, ShouldEqual, 1)
			So(q2.Archived, ShouldEqual, 1)
			So(q2.Groups, ShouldEqual, 1)
			So(q2.Paused, ShouldBeTrue)
			So(q2.PausedSince.IsZero(), ShouldBeFalse)
			So(q2.ProcessedToday, ShouldEqual, 0) // no server has run anything
			So(q2.ErrorRate(), ShouldEqual, 0)
		})

		Convey("Then the fleet aggregate sums and counts correctly", func() {
			f := res.Fleet
			So(f.QueuesTotal, ShouldEqual, 2)
			So(f.Pending, ShouldEqual, 4)
			So(f.Scheduled, ShouldEqual, 2)
			So(f.Archived, ShouldEqual, 1)
			So(f.Groups, ShouldEqual, 1)
			So(f.OrphanCandidates, ShouldEqual, 1)
			So(f.PastDueScheduled, ShouldEqual, 1)
			So(f.PausedQueues, ShouldEqual, 1)
			So(f.ZeroConsumerQueues, ShouldEqual, 2)
			So(f.Servers, ShouldEqual, 0)
			So(f.WorkersTotal, ShouldEqual, 0)
			So(f.RefreshedAt.IsZero(), ShouldBeFalse)
		})

		Convey("Then the sweep cost matches the budget: 15 commands per queue, +1 per queue with pending, +3 per sweep, + bounded group reads", func() {
			cost := engine.LastSweep()
			So(cost.Queues, ShouldEqual, 2)
			// 1 SMEMBERS asynq:queues + 1 SMEMBERS cache index
			// + 2 queues x 15 hot-read commands (§5.1's 14 + the phase-4
			//   RETRY_STORM ZCOUNT)
			// + 2 pending_since hops (both queues have pending tasks)
			// + 1 INFO memory
			// + 2 GROUP_STALL reads (q2 has one group: SMEMBERS + ZRANGE)
			So(cost.ReadCmds, ShouldEqual, 37)
			// 2 queues x (HSET + PEXPIRE) + 1 SADD index + fleet HSET +
			// PEXPIRE + 1 SET attention report
			So(cost.WriteCmds, ShouldEqual, 8)
			So(cost.Duration, ShouldBeGreaterThan, 0)
		})

		Convey("Then the fleet snapshot carries the Redis memory gauge from INFO", func() {
			So(res.Fleet.RedisMemoryUsed, ShouldBeGreaterThan, 0)
			// maxmemory is whatever the test Redis is configured with; the
			// value only needs to decode, not be nonzero.
			So(res.Fleet.RedisMemoryMax, ShouldBeGreaterThanOrEqualTo, 0)
		})

		Convey("Then a standby replica (no lease, empty memory) reads identical data from the shared cache", func() {
			standby := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
			got, err := standby.Read(ctx)
			So(err, ShouldBeNil)
			So(got.Source, ShouldEqual, SourceCache)
			So(got.Queues, ShouldHaveLength, 2)
			var q1 *QueueSnapshot
			for _, s := range got.Queues {
				if s.Queue == "q1" {
					q1 = s
				}
			}
			So(q1, ShouldNotBeNil)
			local := byName["q1"]
			So(q1.Pending, ShouldEqual, local.Pending)
			So(q1.Scheduled, ShouldEqual, local.Scheduled)
			So(q1.PastDueScheduled, ShouldEqual, local.PastDueScheduled)
			So(q1.OrphanCandidates, ShouldEqual, local.OrphanCandidates)
			So(q1.OldestPendingSince.UnixNano(), ShouldEqual, local.OldestPendingSince.UnixNano())
			So(q1.RefreshedAt.UnixNano(), ShouldEqual, local.RefreshedAt.UnixNano())
			So(got.Fleet.Pending, ShouldEqual, res.Fleet.Pending)
			So(got.Fleet.ZeroConsumerQueues, ShouldEqual, res.Fleet.ZeroConsumerQueues)
		})

		// Keep this leaf last: it mutates the fleet.
		Convey("When a queue is deleted and the next sweep runs", func() {
			So(insp.DeleteQueue("q1", true), ShouldBeNil)
			So(engine.SweepNow(ctx), ShouldBeNil)
			Convey("Then its cache entry and index membership are removed", func() {
				So(rc.Exists(ctx, queueCacheKey("q1")).Val(), ShouldEqual, 0)
				So(rc.SIsMember(ctx, queueIndexKey, "q1").Val(), ShouldBeFalse)
				after, err := engine.Read(ctx)
				So(err, ShouldBeNil)
				So(after.Queues, ShouldHaveLength, 1)
				So(after.Queues[0].Queue, ShouldEqual, "q2")
				So(after.Fleet.QueuesTotal, ShouldEqual, 1)
			})
		})
	})
}

// TestSweepServerDerived runs a real asynq server so retry state, daily
// counters, completed tasks and consumer counts are produced by asynq's own
// machinery.
func TestSweepServerDerived(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	srv := asynq.NewServer(testAsynqOpt(), asynq.Config{
		Concurrency: 2,
		Queues:      map[string]int{"q3": 1},
		LogLevel:    asynq.FatalLevel,
		// Push the retry far out so the task stays in retry state (and its
		// next_retry_at is unambiguously in the future) for the whole test.
		RetryDelayFunc: func(n int, e error, task *asynq.Task) time.Duration { return time.Hour },
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc("job:fail", func(context.Context, *asynq.Task) error { return errors.New("boom") })
	mux.HandleFunc("job:ok", func(context.Context, *asynq.Task) error { return nil })
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting asynq server: %v", err)
	}
	defer srv.Shutdown()

	if _, err := client.Enqueue(asynq.NewTask("job:fail", nil), asynq.Queue("q3"), asynq.MaxRetry(3)); err != nil {
		t.Fatalf("enqueue failing task: %v", err)
	}
	if _, err := client.Enqueue(asynq.NewTask("job:ok", nil), asynq.Queue("q3"), asynq.Retention(time.Hour)); err != nil {
		t.Fatalf("enqueue succeeding task: %v", err)
	}
	waitFor(t, 15*time.Second, "one task in retry and one completed", func() bool {
		qi, err := insp.GetQueueInfo("q3")
		return err == nil && qi.Retry == 1 && qi.Completed == 1
	})

	engine := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
	sweepErr := engine.SweepNow(ctx)
	res, readErr := engine.Read(ctx)
	beforeSweep := time.Now()

	Convey("Given a live worker that failed one task and completed another", t, func() {
		So(sweepErr, ShouldBeNil)
		So(readErr, ShouldBeNil)
		So(res.Queues, ShouldHaveLength, 1)
		q3 := res.Queues[0]

		Convey("Then retry, completed and next_retry_at come from asynq's own writes", func() {
			So(q3.Retry, ShouldEqual, 1)
			So(q3.Completed, ShouldEqual, 1)
			So(q3.Pending, ShouldEqual, 0)
			So(q3.NextRetryAt.After(beforeSweep.Add(30*time.Minute)), ShouldBeTrue)
			// The retry fires in ~1h, far outside the 5m storm window — the
			// RETRY_STORM mass must NOT count it.
			So(q3.RetryDueSoon, ShouldEqual, 0)
		})

		Convey("Then daily counters count both outcomes (processed includes failures)", func() {
			So(q3.ProcessedToday, ShouldEqual, 2)
			So(q3.FailedToday, ShouldEqual, 1)
			So(q3.ErrorRate(), ShouldEqual, 0.5)
			// And the mirrored daily-counter key formats point at asynq's
			// real counters.
			So(rc.Get(ctx, processedKey("q3", time.Now())).Val(), ShouldEqual, "2")
			So(rc.Get(ctx, failedKey("q3", time.Now())).Val(), ShouldEqual, "1")
		})

		Convey("Then consumer counts and worker totals derive from the live server", func() {
			So(q3.Consumers, ShouldEqual, 1)
			So(res.Fleet.Servers, ShouldEqual, 1)
			So(res.Fleet.WorkersTotal, ShouldEqual, 2)
			So(res.Fleet.ZeroConsumerQueues, ShouldEqual, 0)
		})
	})
}

// TestLeaseFailover exercises the §5.13 singleton model: one sweeper, losers
// serving from the cache, immediate handover on shutdown.
func TestLeaseFailover(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	if _, err := client.Enqueue(asynq.NewTask("t:one", nil), asynq.Queue("lq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	cfg := func(id string) Config {
		return Config{
			RedisClient: rc,
			Inspector:   insp,
			Interval:    50 * time.Millisecond,
			LeaseTTL:    600 * time.Millisecond,
			InstanceID:  id,
			Logf:        silentLogf,
		}
	}

	// Single-path Convey (no sibling leaves): the stateful sequence below
	// must execute exactly once.
	Convey("Given two replicas sharing one Redis", t, func() {
		eng1 := NewEngine(cfg("replica-1"))
		eng2 := NewEngine(cfg("replica-2"))
		Reset(func() {
			eng1.Stop()
			eng2.Stop()
		})

		Convey("When the first replica starts", func() {
			eng1.Start(ctx)
			So(eventually(5*time.Second, eng1.LeaseHeld), ShouldBeTrue)
			So(eventually(5*time.Second, func() bool { return !eng1.LastSweep().At.IsZero() }), ShouldBeTrue)

			Convey("And a second replica starts while the lease is held", func() {
				eng2.Start(ctx)
				time.Sleep(700 * time.Millisecond) // several lease ticks
				So(eng2.LeaseHeld(), ShouldBeFalse)

				res, err := eng2.Read(ctx)
				So(err, ShouldBeNil)
				So(res.Source, ShouldEqual, SourceCache)
				So(res.Fleet.QueuesTotal, ShouldEqual, 1)
				So(res.Queues[0].Queue, ShouldEqual, "lq")

				Convey("When the lease holder stops", func() {
					eng1.Stop()
					So(eventually(5*time.Second, eng2.LeaseHeld), ShouldBeTrue)
					So(eventually(5*time.Second, func() bool { return !eng2.LastSweep().At.IsZero() }), ShouldBeTrue)
					local, err := eng2.Read(ctx)
					So(err, ShouldBeNil)
					So(local.Source, ShouldEqual, SourceLocal)
					So(local.Fleet.QueuesTotal, ShouldEqual, 1)
				})
			})
		})
	})
}

// TestEngineLifecycle covers clean start/stop semantics: idempotent Stop,
// restartability, lease release on shutdown, and (under -race, with the
// wg.Wait inside Stop) goroutine hygiene.
func TestEngineLifecycle(t *testing.T) {
	rc := testRedis(t)
	_, insp := testClientInspector(t)
	ctx := context.Background()

	Convey("Given an engine with a short interval", t, func() {
		eng := NewEngine(Config{
			RedisClient: rc,
			Inspector:   insp,
			Interval:    50 * time.Millisecond,
			LeaseTTL:    600 * time.Millisecond,
			Logf:        silentLogf,
		})
		Reset(eng.Stop)

		Convey("When started twice and stopped twice", func() {
			eng.Start(ctx)
			eng.Start(ctx) // no-op
			So(eventually(5*time.Second, eng.LeaseHeld), ShouldBeTrue)
			eng.Stop()
			eng.Stop() // idempotent

			Convey("Then the lease is released for the next holder", func() {
				So(rc.Exists(ctx, sweeperLockKey).Val(), ShouldEqual, 0)
				So(eng.LeaseHeld(), ShouldBeFalse)

				Convey("And the engine can be started again", func() {
					eng.Start(ctx)
					So(eventually(5*time.Second, eng.LeaseHeld), ShouldBeTrue)
					eng.Stop()
				})
			})
		})

		Convey("When stopped without ever being started", func() {
			fresh := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
			So(fresh.Stop, ShouldNotPanic)
		})
	})
}
