package stats

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Phase-12 integration tests on DB 14 (the stats package's own database, see
// engine_test.go): shard rotation under a tiny command budget, hot-set
// refresh every tick, viewed-queue plumbing, and the sweeper's fenced write
// path. Clocks are injected so slot/staleness math is deterministic.
// ****************************************************************************

// TestRotationRefreshesFleetWithinWindow simulates a 100-queue fleet forced
// into tier 2 (test ceiling override) under a tiny budget and asserts the
// §5.1 rotation contract: every queue refreshed within the computed window,
// the hot set on every tick.
func TestRotationRefreshesFleetWithinWindow(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	// 100 queues, one pending task each; q090-q099 get deep backlogs so the
	// hot set (top-K by backlog) is deterministic once observed.
	for i := 0; i < 100; i++ {
		q := fmt.Sprintf("q%03d", i)
		n := 1
		if i >= 90 {
			n = 20 + (i - 90)
		}
		for j := 0; j < n; j++ {
			if _, err := client.Enqueue(asynq.NewTask("t:load", nil), asynq.Queue(q)); err != nil {
				t.Fatalf("enqueue %s: %v", q, err)
			}
		}
	}

	// Injected clock: each SweepNow is one 5s tick.
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Budget 96 cmds/sec × 5s = 480 cmds/tick = 30 refreshes/tick (cost 16),
	// minus the hot set (K=4) once it forms.
	eng := NewEngine(Config{
		RedisClient:   rc,
		Inspector:     insp,
		Interval:      5 * time.Second,
		CommandBudget: 96,
		HotSetK:       4,
		tier1Ceiling:  10, // force 100 queues into tier 2
		Now:           clock,
		Logf:          silentLogf,
	})

	// Tick 1..4: unseen-first rotation observes the whole fleet (100/30 → 4
	// ticks).
	refreshedAt := map[string]time.Time{}
	tick := func() {
		if err := eng.SweepNow(ctx); err != nil {
			t.Fatalf("sweep at %v: %v", now, err)
		}
		res, err := eng.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		for _, s := range res.Queues {
			refreshedAt[s.Queue] = s.RefreshedAt
		}
		now = now.Add(5 * time.Second)
	}

	tick()
	afterFirst := len(refreshedAt)
	firstRes, _ := eng.Read(ctx)
	firstStats := eng.LastSweep()

	// Phase A: tick until the unseen-first rotation has observed the whole
	// fleet (bounded), so the hot set and rotation reach steady state. The
	// fixture has no live workers, so every queue raises NO_CONSUMERS after
	// its 2-sweep debounce; three settle ticks let the LAST-observed queues'
	// findings mature so the severity-ranked attention component of the hot
	// set is stable before the window measurement starts.
	warmup := 1
	for len(refreshedAt) < 100 && warmup < 40 {
		tick()
		warmup++
	}
	observedAll := len(refreshedAt)
	for i := 0; i < 3; i++ {
		tick()
	}

	// Phase B: one full steady-state rotation window, as computed by the
	// engine itself — the §5.1 contract is "every queue refreshed within
	// N×cost/budget seconds" and the engine publishes that window as
	// FullRotationEstSec (the attention-flagged hot set shrinks the cold
	// shard, so the window is wider than naive budget/cost math).
	steadyRes, _ := eng.Read(ctx)
	rotationSec := steadyRes.Fleet.FullRotationEstSec
	windowTicks := rotationSec/5 + 1
	startStamps := map[string]time.Time{}
	for _, s := range steadyRes.Queues {
		startStamps[s.Queue] = s.RefreshedAt
	}
	refreshEvents := map[string]int{}
	for i := 0; i < windowTicks; i++ {
		before := map[string]time.Time{}
		res, _ := eng.Read(ctx)
		for _, s := range res.Queues {
			before[s.Queue] = s.RefreshedAt
		}
		tick()
		res, _ = eng.Read(ctx)
		for _, s := range res.Queues {
			if !s.RefreshedAt.Equal(before[s.Queue]) {
				refreshEvents[s.Queue]++
			}
		}
	}
	finalRes, _ := eng.Read(ctx)
	finalStats := eng.LastSweep()

	Convey("Given a 100-queue tier-2 fleet under a 96 cmd/s budget", t, func() {
		Convey("Then the first tick refreshes only one budget shard, not the fleet", func() {
			So(afterFirst, ShouldBeLessThan, 100)
			So(afterFirst, ShouldBeGreaterThan, 0)
			So(firstStats.Tier, ShouldEqual, 2)
			So(firstStats.Refreshed, ShouldEqual, afterFirst)
			// Shard spend honors the budget: refreshed × cost ≤ budget × interval.
			So(firstStats.Refreshed*perQueueSweepCost, ShouldBeLessThanOrEqualTo, 96*5)
			So(firstRes.Fleet.Tier, ShouldEqual, 2)
		})

		Convey("Then the unseen-first rotation observes the whole fleet", func() {
			So(observedAll, ShouldEqual, 100)
		})

		Convey("Then one computed rotation window refreshes every queue", func() {
			So(rotationSec, ShouldBeGreaterThan, 0)
			for _, s := range finalRes.Queues {
				So(s.RefreshedAt.After(startStamps[s.Queue]) ||
					refreshEvents[s.Queue] > 0, ShouldBeTrue)
			}
			So(len(refreshEvents), ShouldEqual, 100)
		})

		Convey("Then the coverage metadata is published for the stamps", func() {
			So(finalRes.Fleet.CommandBudget, ShouldEqual, 96)
			So(finalRes.Fleet.HotSetSize, ShouldBeGreaterThanOrEqualTo, 4)
			So(finalRes.Fleet.FullRotationEstSec, ShouldBeGreaterThan, 0)
			// Sanity bound: 96 cold queues at the minimum 1-per-tick quantum
			// would be 480s; the real shard is far bigger.
			So(finalRes.Fleet.FullRotationEstSec, ShouldBeLessThanOrEqualTo, 240)
		})

		Convey("Then the hot set (top-K backlog) refreshed on every tick of the window", func() {
			// q096-q099 carry the deepest backlogs (26-29 pending).
			for _, q := range []string{"q096", "q097", "q098", "q099"} {
				So(refreshEvents[q], ShouldEqual, windowTicks)
			}
			So(finalStats.HotSetSize, ShouldBeGreaterThanOrEqualTo, 4)
		})

		Convey("Then only the hot set refreshes every tick — the cold majority rotates", func() {
			everyTickCount := 0
			for _, n := range refreshEvents {
				if n == windowTicks {
					everyTickCount++
				}
			}
			// Every-tick refreshers are (at most) the hot set; the rest of the
			// fleet is on rotation cadence.
			So(everyTickCount, ShouldBeLessThanOrEqualTo, finalStats.HotSetSize)
			So(100-everyTickCount, ShouldBeGreaterThan, 50)
		})

		Convey("Then every published row still carries its honest RefreshedAt", func() {
			stale := 0
			for _, s := range finalRes.Queues {
				So(s.RefreshedAt.IsZero(), ShouldBeFalse)
				if now.Sub(s.RefreshedAt) > 5*time.Second {
					stale++
				}
			}
			// Most of the fleet is carried forward between visits.
			So(stale, ShouldBeGreaterThan, 0)
		})
	})
}

// TestRotationViewedQueueJoinsHotSet covers the viewed-queue bridge: a queue
// marked viewed (directory name= filter / workspace endpoints) is refreshed
// every tick while the view is recent.
func TestRotationViewedQueueJoinsHotSet(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	for i := 0; i < 40; i++ {
		if _, err := client.Enqueue(asynq.NewTask("t:x", nil), asynq.Queue(fmt.Sprintf("vq%02d", i))); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	eng := NewEngine(Config{
		RedisClient:   rc,
		Inspector:     insp,
		Interval:      5 * time.Second,
		CommandBudget: 32, // 160 cmds/tick = 10 refreshes
		HotSetK:       1,
		tier1Ceiling:  10,
		Now:           clock,
		Logf:          silentLogf,
	})
	tracker := NewViewTracker(rc, clock)

	// Observe the fleet once (4 ticks), then mark vq25 viewed.
	for i := 0; i < 4; i++ {
		if err := eng.SweepNow(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		now = now.Add(5 * time.Second)
	}
	tracker.MarkViewed(ctx, "vq25")

	refreshCount := 0
	const ticks = 3
	for i := 0; i < ticks; i++ {
		var before time.Time
		res, _ := eng.Read(ctx)
		for _, s := range res.Queues {
			if s.Queue == "vq25" {
				before = s.RefreshedAt
			}
		}
		if err := eng.SweepNow(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		res, _ = eng.Read(ctx)
		for _, s := range res.Queues {
			if s.Queue == "vq25" && !s.RefreshedAt.Equal(before) {
				refreshCount++
			}
		}
		now = now.Add(5 * time.Second)
	}

	Convey("Given a tier-2 fleet with one recently-viewed queue", t, func() {
		Convey("Then the viewed queue is refreshed on every tick while the view is recent", func() {
			// 3 ticks fit inside the 60s viewed window (views age out after).
			So(refreshCount, ShouldEqual, ticks)
		})
	})
}

// TestSweeperFencing covers the §5.13 fenced write path: a sweeper whose
// fencing token was superseded by a newer claimant cannot publish, and
// stands down.
func TestSweeperFencing(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	if _, err := client.Enqueue(asynq.NewTask("t:one", nil), asynq.Queue("fq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	eng := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})

	// Claim the sweeper role as this engine would (token1), then simulate a
	// newer claimant taking over after a lease loss (release + re-acquire
	// mints token2 > token1).
	token1, err := eng.fence.Acquire(ctx, eng.InstanceID(), time.Minute)
	if err != nil || token1 == 0 {
		t.Fatalf("acquiring role: token=%d err=%v", token1, err)
	}
	atomic.StoreInt64(&eng.token, token1)
	atomic.StoreInt32(&eng.holding, 1)

	if err := eng.fence.Release(ctx, eng.InstanceID()); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	token2, err := eng.fence.Acquire(ctx, "newer-replica", time.Minute)
	if err != nil || token2 <= token1 {
		t.Fatalf("newer claim: token=%d err=%v", token2, err)
	}

	sweepErr := eng.SweepNow(ctx)

	Convey("Given a sweeper holding a superseded fencing token", t, func() {
		Convey("Then the sweep reports supersession", func() {
			So(errors.Is(sweepErr, ErrSuperseded), ShouldBeTrue)
		})
		Convey("Then the stale holder's cache publish was rejected", func() {
			So(rc.Exists(ctx, fleetCacheKey).Val(), ShouldEqual, 0)
			So(rc.Exists(ctx, queueCacheKey("fq")).Val(), ShouldEqual, 0)
		})
		Convey("Then the ex-holder stood down", func() {
			So(eng.LeaseHeld(), ShouldBeFalse)
			So(atomic.LoadInt64(&eng.token), ShouldEqual, 0)
		})
		Convey("And an explicitly off-lease SweepNow (token 0) still writes — the documented operational path", func() {
			standby := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
			So(standby.SweepNow(ctx), ShouldBeNil)
			So(rc.Exists(ctx, fleetCacheKey).Val(), ShouldEqual, 1)
		})
	})
}
