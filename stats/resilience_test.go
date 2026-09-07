package stats

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Redis-write-failure resilience (#36) and the shutdown series flush (#28).
//
// The write outage is produced the way an operator sees it: CONFIG SET
// maxmemory 1 with policy noeviction, so every write returns OOM while reads
// keep working. Both settings are restored in t.Cleanup. When CONFIG SET is
// unavailable (a managed Redis), the test falls back to a client wrapper
// that fails every write.
//
// This file uses the package's DB 14 (see engine_test.go).
// ****************************************************************************

// forceOOM makes every write to rc fail with an OOM error. It returns the
// restore func and true, or false when the server does not allow CONFIG SET.
// maxmemory is a SERVER setting, so the caller restores it as soon as the
// outage window ends; t.Cleanup is only the backstop.
func forceOOM(t *testing.T, rc redis.UniversalClient) (func(), bool) {
	t.Helper()
	ctx := context.Background()
	maxmemory, err := rc.ConfigGet(ctx, "maxmemory").Result()
	if err != nil {
		return nil, false
	}
	policy, err := rc.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		return nil, false
	}
	restore := func() {
		c := redis.NewClient(&redis.Options{Addr: testRedisAddr, DB: testRedisDB})
		defer c.Close()
		c.ConfigSet(context.Background(), "maxmemory", maxmemory["maxmemory"])
		c.ConfigSet(context.Background(), "maxmemory-policy", policy["maxmemory-policy"])
	}
	if err := rc.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err(); err != nil {
		return nil, false
	}
	if err := rc.ConfigSet(ctx, "maxmemory", "1").Err(); err != nil {
		restore()
		return nil, false
	}
	t.Cleanup(restore)
	return restore, true
}

// TestSweepSurvivesWriteOutage proves the holder keeps answering from memory
// during a total write outage, and that a second replica reading the shared
// cache degrades to stale-cache instead of ErrNotReady (the 503 the reviewer
// measured after the 2m cache TTL).
func TestSweepSurvivesWriteOutage(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	if _, err := client.Enqueue(asynq.NewTask("t:one", nil), asynq.Queue("oomq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	holder := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
	standby := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})

	// One healthy sweep so both replicas have seen real data.
	if err := holder.SweepNow(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	firstStandby, err := standby.Read(ctx)
	if err != nil {
		t.Fatalf("standby first read: %v", err)
	}

	restore, ok := forceOOM(t, rc)
	if !ok {
		t.Skip("skipping: server does not allow CONFIG SET maxmemory")
	}

	// A sweep during the outage: every write fails, every read still works.
	oomSweepErr := holder.SweepNow(ctx)
	holderRead, holderErr := holder.Read(ctx)
	restore() // keep the server-wide outage window as short as possible

	// The shared cache expired because no replica could refresh it — the 2m
	// CacheTTL, reproduced with a DEL.
	rc.Del(ctx, fleetCacheKey)
	staleRead, staleErr := standby.Read(ctx)

	fresh := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
	_, freshErr := fresh.Read(ctx)

	Convey("Given a Redis that rejects every write with OOM", t, func() {
		Convey("Then the sweep reports the write failure", func() {
			So(oomSweepErr, ShouldNotBeNil)
			So(oomSweepErr.Error(), ShouldContainSubstring, "OOM")
		})

		Convey("Then the holder still serves its own sweep from memory", func() {
			So(holderErr, ShouldBeNil)
			So(holderRead.Source, ShouldEqual, SourceLocal)
			So(holderRead.Fleet.QueuesTotal, ShouldEqual, 1)
			So(holderRead.Queues[0].Queue, ShouldEqual, "oomq")
		})

		Convey("Then a replica that read the cache before serves it as stale-cache, not 503", func() {
			So(firstStandby.Source, ShouldEqual, SourceCache)
			So(staleErr, ShouldBeNil)
			So(staleRead.Source, ShouldEqual, SourceStaleCache)
			So(staleRead.Fleet.QueuesTotal, ShouldEqual, 1)
			So(staleRead.Fleet.RefreshedAt.Equal(firstStandby.Fleet.RefreshedAt), ShouldBeTrue)
		})

		Convey("Then a replica that never read anything still reports not-ready", func() {
			So(errors.Is(freshErr, ErrNotReady), ShouldBeTrue)
		})
	})
}

// TestStopFlushesSeries proves #28: Stop writes the queued slots and the
// partial accumulators before it releases the lease.
func TestStopFlushesSeries(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	if _, err := client.Enqueue(asynq.NewTask("t:one", nil), asynq.Queue("flushq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	eng := NewEngine(Config{
		RedisClient: rc,
		Inspector:   insp,
		Interval:    50 * time.Millisecond,
		LeaseTTL:    2 * time.Second,
		Logf:        silentLogf,
	})
	eng.Start(ctx)
	waitFor(t, 5*time.Second, "the lease and one sweep", func() bool {
		return eng.LeaseHeld() && !eng.LastSweep().At.IsZero()
	})

	fleetKey := seriesKey(hotRing, seriesKeyScopeFleet(), MetricPending)
	beforeLen := rc.StrLen(ctx, fleetKey).Val()
	eng.Stop()
	afterLen := rc.StrLen(ctx, fleetKey).Val()
	raw := rc.Get(ctx, fleetKey).Val()

	Convey("Given a sweeper stopped while its hot slot is still open", t, func() {
		Convey("Then no slot had been written before Stop", func() {
			So(beforeLen, ShouldEqual, 0)
		})
		Convey("Then Stop wrote the partial slot through the fence", func() {
			So(afterLen, ShouldEqual, int64(hotRing.byteLen()))
			h := decodeSeriesHeader([]byte(raw))
			So(h.ok, ShouldBeTrue)
			So(decodeSeriesPoint([]byte(raw), h, hotRing, h.last), ShouldNotBeNil)
		})
	})
}

func TestErrorLimiter(t *testing.T) {
	Convey("Given the sweep-error log limiter", t, func() {
		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		l := newErrorLimiter(func() time.Time { return now })
		boom := errors.New("OOM command not allowed")

		Convey("When the same error repeats within a minute", func() {
			first, ok1 := l.observe("sweep", boom)
			now = now.Add(30 * time.Second)
			_, ok2 := l.observe("sweep", boom)
			now = now.Add(1 * time.Second)
			_, ok3 := l.observe("sweep", boom)

			Convey("Then only the first occurrence is logged", func() {
				So(ok1, ShouldBeTrue)
				So(first, ShouldContainSubstring, "OOM command not allowed")
				So(ok2, ShouldBeFalse)
				So(ok3, ShouldBeFalse)
			})

			Convey("And the next line comes after a minute, with the count", func() {
				now = now.Add(time.Minute)
				msg, ok := l.observe("sweep", boom)
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "4 times")
			})

			Convey("And a recovery is logged exactly once", func() {
				msg, ok := l.observe("sweep", nil)
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "recovered")
				_, ok = l.observe("sweep", nil)
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When a different error arrives", func() {
			l.observe("sweep", boom)
			msg, ok := l.observe("sweep", fmt.Errorf("connection refused"))

			Convey("Then it is logged immediately, not rate-limited", func() {
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "connection refused")
			})
		})

		Convey("When two subsystems fail", func() {
			_, ok1 := l.observe("series", boom)
			_, ok2 := l.observe("schedulers", boom)

			Convey("Then each subsystem gets its own first line", func() {
				So(ok1, ShouldBeTrue)
				So(ok2, ShouldBeTrue)
			})
		})
	})
}
