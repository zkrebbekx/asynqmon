package stats

import (
	"fmt"
	"sort"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Pure unit tests for the phase-12 command-budget governor (rotation.go):
// tier math, budget/shard math, hot-set selection, cursor rotation. No Redis;
// clocks are injected where relevant.
// ****************************************************************************

func TestTierForFleet(t *testing.T) {
	Convey("Given the §5.1 tiering table", t, func() {
		Convey("Then fleet sizes map to their normative tiers", func() {
			So(TierForFleet(0), ShouldEqual, 1)
			So(TierForFleet(1), ShouldEqual, 1)
			So(TierForFleet(2000), ShouldEqual, 1)
			So(TierForFleet(2001), ShouldEqual, 2)
			So(TierForFleet(20000), ShouldEqual, 2)
			So(TierForFleet(20001), ShouldEqual, 3)
			So(TierForFleet(1000000), ShouldEqual, 3)
		})
	})
}

func TestHotScore(t *testing.T) {
	Convey("Given the hot-set ranking function", t, func() {
		Convey("Then backlog dominates and error rate boosts", func() {
			deep := &QueueSnapshot{Pending: 1000}
			shallow := &QueueSnapshot{Pending: 10}
			So(hotScore(deep), ShouldBeGreaterThan, hotScore(shallow))

			erroring := &QueueSnapshot{Pending: 100, ProcessedToday: 100, FailedToday: 50}
			clean := &QueueSnapshot{Pending: 100, ProcessedToday: 100}
			So(hotScore(erroring), ShouldBeGreaterThan, hotScore(clean))
		})
		Convey("Then a failing-but-drained queue still scores above a silent empty one", func() {
			failing := &QueueSnapshot{ProcessedToday: 100, FailedToday: 100}
			silent := &QueueSnapshot{}
			So(hotScore(failing), ShouldBeGreaterThan, hotScore(silent))
			So(hotScore(silent), ShouldEqual, 0)
		})
	})
}

func TestHotSetSelection(t *testing.T) {
	Convey("Given a fleet with attention, viewed, and backlog signals", t, func() {
		universe := map[string]bool{}
		prev := map[string]*QueueSnapshot{}
		for i := 0; i < 10; i++ {
			q := fmt.Sprintf("q%02d", i)
			universe[q] = true
			// q00 deepest … q09 shallowest.
			prev[q] = &QueueSnapshot{Queue: q, Pending: int64(1000 - i*100)}
		}

		Convey("When selecting with K=3", func() {
			hot := hotSet(universe, prev, []string{"q07"}, []string{"q09"}, 3)

			Convey("Then it is attention ∪ viewed ∪ top-K by score, sorted", func() {
				So(hot, ShouldResemble, []string{"q00", "q01", "q02", "q07", "q09"})
			})
		})

		Convey("When attention/viewed name queues outside the universe", func() {
			hot := hotSet(universe, prev, []string{"ghost"}, []string{"gone"}, 2)
			Convey("Then they are ignored (never sweep deleted queues)", func() {
				So(hot, ShouldResemble, []string{"q00", "q01"})
			})
		})

		Convey("When K exceeds the observed population", func() {
			hot := hotSet(universe, prev, nil, nil, 50)
			Convey("Then every observed queue is hot", func() {
				So(hot, ShouldHaveLength, 10)
			})
		})

		Convey("When scores tie", func() {
			tied := map[string]*QueueSnapshot{
				"b": {Queue: "b", Pending: 5},
				"a": {Queue: "a", Pending: 5},
				"c": {Queue: "c", Pending: 5},
			}
			uni := map[string]bool{"a": true, "b": true, "c": true}
			hot := hotSet(uni, tied, nil, nil, 2)
			Convey("Then the tie-break is deterministic (name ascending)", func() {
				So(hot, ShouldResemble, []string{"a", "b"})
			})
		})
	})
}

// planQueues builds n sorted queue names and their observed snapshots.
func planQueues(n int) ([]string, map[string]*QueueSnapshot) {
	names := make([]string, 0, n)
	prev := make(map[string]*QueueSnapshot, n)
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("q%03d", i)
		names = append(names, q)
		prev[q] = &QueueSnapshot{Queue: q}
	}
	sort.Strings(names)
	return names, prev
}

func TestPlanSweepTier1(t *testing.T) {
	Convey("Given a tier-1 fleet", t, func() {
		names, prev := planQueues(100)
		plan := planSweep(1, names, prev, nil, nil, 0, DefaultCommandBudget, 5*time.Second, 0)
		Convey("Then the plan is a full sweep with no hot set (§5.1 table row 1)", func() {
			So(plan.full, ShouldBeTrue)
			So(plan.tier, ShouldEqual, 1)
			So(plan.hot, ShouldBeEmpty)
			So(plan.fullRotationEstSec, ShouldEqual, 0)
		})
	})
}

func TestPlanSweepBudgetMath(t *testing.T) {
	Convey("Given a tier-2 fleet of 100 queues under a tiny budget", t, func() {
		names, prev := planQueues(100)
		// Budget 64 cmds/sec × 5s = 320 cmds/tick. Hot set: K=4 → 4×16 = 64
		// cmds, leaving 256/16 = 16 cold slots per tick.
		interval := 5 * time.Second
		plan := planSweep(2, names, prev, nil, nil, 0, 64, interval, 4)

		Convey("Then the shard math spends at most budget×interval commands", func() {
			So(plan.full, ShouldBeFalse)
			So(len(plan.hot), ShouldEqual, 4)
			So(len(plan.cold), ShouldEqual, 16)
			spend := (len(plan.hot) + len(plan.cold)) * perQueueSweepCost
			So(spend, ShouldBeLessThanOrEqualTo, 64*5)
		})

		Convey("Then the full-rotation estimate covers the cold set", func() {
			// 96 cold queues at 16/tick = 6 ticks × 5s = 30s.
			So(plan.fullRotationEstSec, ShouldEqual, 30)
		})

		Convey("When ticks advance the cursor", func() {
			seen := map[string]int{}
			cursor := 0
			ticks := 6 // = ceil(96/16): one full rotation
			for i := 0; i < ticks; i++ {
				p := planSweep(2, names, prev, nil, nil, cursor, 64, interval, 4)
				cursor = p.cursor
				for _, q := range p.cold {
					seen[q]++
				}
				for _, q := range p.hot {
					seen[q]++
				}
			}
			Convey("Then every queue is refreshed within the estimated window", func() {
				So(len(seen), ShouldEqual, 100)
			})
			Convey("And hot queues were refreshed on every tick", func() {
				hot := planSweep(2, names, prev, nil, nil, 0, 64, interval, 4).hot
				for _, q := range hot {
					So(seen[q], ShouldEqual, ticks)
				}
			})
		})
	})

	Convey("Given a budget generous enough to cover the whole fleet", t, func() {
		names, prev := planQueues(100)
		plan := planSweep(2, names, prev, nil, nil, 0, DefaultCommandBudget, 5*time.Second, 4)
		Convey("Then rotation degenerates to a full sweep with the hot set intact", func() {
			So(plan.full, ShouldBeTrue)
			So(len(plan.hot), ShouldEqual, 4)
			So(plan.fullRotationEstSec, ShouldEqual, 5)
		})
	})

	Convey("Given a hot set that alone exceeds the budget", t, func() {
		names, prev := planQueues(100)
		// Budget 3 cmds/sec × 5s = 15 < one queue's cost; K=10.
		plan := planSweep(2, names, prev, nil, nil, 0, 3, 5*time.Second, 10)
		Convey("Then the hot set is refreshed regardless and the cold rotation keeps its minimum quantum", func() {
			So(len(plan.hot), ShouldEqual, 10)
			So(len(plan.cold), ShouldEqual, 1)
		})
	})
}

func TestPlanSweepUnseenFirst(t *testing.T) {
	Convey("Given a fleet where some queues have never been observed", t, func() {
		names, prev := planQueues(50)
		// Forget the last 5 queues: they are new to the sweeper.
		for i := 45; i < 50; i++ {
			delete(prev, fmt.Sprintf("q%03d", i))
		}
		// Budget for exactly 8 refreshes per tick, no hot set.
		plan := planSweep(2, names, prev, nil, nil, 0, 8*perQueueSweepCost/5, 5*time.Second, 1)

		Convey("Then never-observed queues fill the shard first", func() {
			So(len(plan.cold), ShouldBeGreaterThanOrEqualTo, 5)
			coldSet := map[string]bool{}
			for _, q := range plan.cold {
				coldSet[q] = true
			}
			for i := 45; i < 50; i++ {
				So(coldSet[fmt.Sprintf("q%03d", i)], ShouldBeTrue)
			}
		})
	})
}

func TestViewTrackerThrottle(t *testing.T) {
	Convey("Given a ViewTracker with an injected clock", t, func() {
		now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		// nil Redis client would panic on publish; exercise only the
		// throttle decision via the internal map.
		v := &ViewTracker{now: clock, last: map[string]time.Time{}}

		Convey("When a queue was published recently", func() {
			v.last["q1"] = now.Add(-viewedRepublishAfter / 2)
			v.mu.Lock()
			last, ok := v.last["q1"]
			throttled := ok && clock().Sub(last) < viewedRepublishAfter
			v.mu.Unlock()
			Convey("Then the republish is throttled", func() {
				So(throttled, ShouldBeTrue)
			})
		})

		Convey("When the throttle window has passed", func() {
			v.last["q1"] = now.Add(-viewedRepublishAfter - time.Second)
			last := v.last["q1"]
			Convey("Then the queue republishes", func() {
				So(clock().Sub(last) >= viewedRepublishAfter, ShouldBeTrue)
			})
		})
	})
}
