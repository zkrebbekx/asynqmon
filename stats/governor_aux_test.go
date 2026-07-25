package stats

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Regression tests for the auxiliary-budget caps added by the phase-12 scale
// validation (docs/SCALE.md): series slot-boundary flush bursts and cadenced
// baseline loads must not overrun the tick budget in tiers 2-3, and must stay
// byte-identical to pre-cap behavior in tier 1. Real Redis on DB 14 (see
// engine_test.go); clocks injected.
// ****************************************************************************

// auxSnaps builds n refreshed queue snapshots named aq00..aq<n-1>.
func auxSnaps(n int, now time.Time) map[string]*QueueSnapshot {
	snaps := make(map[string]*QueueSnapshot, n)
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("aq%02d", i)
		snaps[q] = &QueueSnapshot{Queue: q, Pending: int64(i + 1), RefreshedAt: now}
	}
	return snaps
}

func auxUniverse(snaps map[string]*QueueSnapshot) map[string]bool {
	u := make(map[string]bool, len(snaps))
	for q := range snaps {
		u[q] = true
	}
	return u
}

// TestSeriesFlushSpread covers the tiers-2-3 flush drain cap: every hot-ring
// key flushes on the same tick when the sweep clock crosses a 30s slot
// boundary, so the sampler must spread that burst across ticks instead of
// spending ~2×keys commands at once (scale validation measured boundary
// ticks at ~1.6× budget without the cap).
func TestSeriesFlushSpread(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()

	// 12:00:07 UTC sits mid-slot; +30s crosses exactly one hot-ring slot
	// boundary while staying inside the same rollup hour.
	t0 := time.Date(2026, 7, 25, 12, 0, 7, 0, time.UTC)
	fleet := &FleetSnapshot{Pending: 10, RefreshedAt: t0}

	Convey("Given a tier-2 sampler with a small flush cap", t, func() {
		s := newSeriesSampler(rc, 0, 3)
		snaps := auxSnaps(4, t0)
		tick := seriesTick{tier: 2, fleetSize: 100, hot: auxUniverse(snaps)}

		_, _, err := s.sample(ctx, t0, snaps, auxUniverse(snaps), tick, fleet, nil)
		So(err, ShouldBeNil)
		So(s.pending, ShouldBeEmpty) // first observation only accumulates

		Convey("When the sweep clock crosses a hot-ring slot boundary", func() {
			_, _, err := s.sample(ctx, t0.Add(30*time.Second), snaps, auxUniverse(snaps), tick, fleet, nil)
			So(err, ShouldBeNil)

			// 7 fleet gauges + 4 queues × 5 gauges = 27 hot-ring keys flushed
			// at the boundary (delta metrics have no first-tick sample; the
			// rollup hour has not rolled). Drain = flat cap = 3.
			Convey("Then only a capped batch is written and the rest queues", func() {
				So(len(s.pending), ShouldEqual, 24)
			})

			Convey("Then subsequent ticks drain the backlog to zero at the flat cap", func() {
				lens := []int{len(s.pending)}
				for i := 0; i < 12 && len(s.pending) > 0; i++ {
					at := t0.Add(30*time.Second + time.Duration(i+1)*time.Second)
					_, _, err := s.sample(ctx, at, snaps, auxUniverse(snaps), tick, fleet, nil)
					So(err, ShouldBeNil)
					lens = append(lens, len(s.pending))
				}
				So(len(s.pending), ShouldEqual, 0)
				// Strictly decreasing while nonempty, never more than the
				// flat cap per tick (the memory-relief valve stays out of
				// reach: backlog ≪ 8×fleetSize).
				for i := 1; i < len(lens); i++ {
					drained := lens[i-1] - lens[i]
					if lens[i-1] > 0 {
						So(drained, ShouldBeGreaterThan, 0)
						So(drained, ShouldBeLessThanOrEqualTo, 3)
					}
				}

				Convey("And the delayed flushes still land as readable points", func() {
					got, err := ReadSeries(ctx, rc, SeriesSpec{
						Scope:  SeriesScope{Kind: "queue", Name: "aq03"},
						Metric: MetricPending,
						Window: SeriesWindow3h,
					}, t0.Add(40*time.Second))
					So(err, ShouldBeNil)
					var found bool
					for _, p := range got.Points {
						if p.V != nil && *p.V == 4 {
							found = true
						}
					}
					So(found, ShouldBeTrue)
				})
			})
		})
	})

	Convey("Given a backlog past the memory-relief valve", t, func() {
		rc.FlushDB(ctx)
		s := newSeriesSampler(rc, 0, 2)
		snaps := auxSnaps(4, t0)
		// fleetSize 1 keeps the valve tiny: max(8×cap, 8×fleetSize) = 16.
		tick := seriesTick{tier: 2, fleetSize: 1, hot: auxUniverse(snaps)}

		_, _, err := s.sample(ctx, t0, snaps, auxUniverse(snaps), tick, fleet, nil)
		So(err, ShouldBeNil)

		Convey("When the boundary queues 27 flushes against a 16-op valve", func() {
			_, _, err := s.sample(ctx, t0.Add(30*time.Second), snaps, auxUniverse(snaps), tick, fleet, nil)
			So(err, ShouldBeNil)

			Convey("Then the excess past the valve drains immediately, the rest at the cap", func() {
				So(len(s.pending), ShouldEqual, 16) // drained 27-16=11 > cap
				_, _, err := s.sample(ctx, t0.Add(31*time.Second), snaps, auxUniverse(snaps), tick, fleet, nil)
				So(err, ShouldBeNil)
				So(len(s.pending), ShouldEqual, 14) // back to flat-cap pace
			})
		})
	})

	Convey("Given cold queues whose rollup slots complete at a UTC hour boundary", t, func() {
		// The hourly hump (docs/SCALE.md): every cold queue's rollup
		// accumulator period advances at the hour, so post-boundary visits
		// queue a fleet-wide wave of rollup flushes. The wave must drain at
		// the flat cap, not at inflow rate.
		rc.FlushDB(ctx)
		s := newSeriesSampler(rc, 0, 3)
		snaps := auxSnaps(6, t0)
		tick := seriesTick{tier: 2, fleetSize: 5000, hot: map[string]bool{}} // all cold → rollup-only

		_, _, err := s.sample(ctx, t0, snaps, auxUniverse(snaps), tick, fleet, nil)
		So(err, ShouldBeNil)
		So(s.pending, ShouldBeEmpty)

		Convey("When the queues are next visited after the hour rolls over", func() {
			_, _, err := s.sample(ctx, t0.Add(time.Hour), snaps, auxUniverse(snaps), tick, fleet, nil)
			So(err, ShouldBeNil)

			Convey("Then the wave queues up and drains at the flat cap per tick", func() {
				So(len(s.pending), ShouldBeGreaterThan, 0)
				prev := len(s.pending)
				for i := 0; i < 20 && len(s.pending) > 0; i++ {
					_, _, err := s.sample(ctx, t0.Add(time.Hour+time.Duration(i+1)*time.Second), snaps, auxUniverse(snaps), tick, fleet, nil)
					So(err, ShouldBeNil)
					So(prev-len(s.pending), ShouldBeLessThanOrEqualTo, 3)
					prev = len(s.pending)
				}
				So(len(s.pending), ShouldEqual, 0)

				Convey("And a cold queue's hourly rollup point landed", func() {
					got, err := ReadSeries(ctx, rc, SeriesSpec{
						Scope:  SeriesScope{Kind: "queue", Name: "aq05"},
						Metric: MetricPending,
						Window: SeriesWindow30d,
					}, t0.Add(time.Hour+30*time.Second))
					So(err, ShouldBeNil)
					var found bool
					for _, p := range got.Points {
						if p.V != nil && *p.V == 6 {
							found = true
						}
					}
					So(found, ShouldBeTrue)
				})
			})
		})
	})

	Convey("Given a tier-1 sampler with the same cap", t, func() {
		rc.FlushDB(ctx)
		s := newSeriesSampler(rc, 0, 3)
		snaps := auxSnaps(4, t0)
		tick := seriesTick{tier: 1, fleetSize: 4}

		_, _, err := s.sample(ctx, t0, snaps, auxUniverse(snaps), tick, fleet, nil)
		So(err, ShouldBeNil)

		Convey("When the slot boundary crosses in tier 1", func() {
			_, _, err := s.sample(ctx, t0.Add(30*time.Second), snaps, auxUniverse(snaps), tick, fleet, nil)
			So(err, ShouldBeNil)

			Convey("Then everything flushes immediately — the budget does not govern tier 1", func() {
				So(s.pending, ShouldBeEmpty)
			})
		})
	})
}

// TestBaselineLoadCaps covers the tiers-2-3 per-sweep baseline command cap:
// "once per UTC day" makes every queue's fail baseline stale on the same
// tick, which at 5,000 queues was a single 35,000-command burst; capped
// sweeps march through the backlog instead.
func TestBaselineLoadCaps(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 7, 0, time.UTC)

	Convey("Given a tier-2 refresher with a 28-command cap over 10 queues", t, func() {
		b := newBaselineRefresher(rc, 28)
		snaps := auxSnaps(10, now)

		Convey("When the first sweep refreshes", func() {
			reads, err := b.refresh(ctx, now, snaps, 2)
			So(err, ShouldBeNil)

			Convey("Then fail baselines load at most cap/7 queues", func() {
				So(len(b.failCache), ShouldEqual, 4)
			})
			Convey("Then the reads stay inside the per-loader caps", func() {
				// 1 anchor + 4×7 fail + min(cap,10) depth + min(cap,10) seed.
				So(reads, ShouldEqual, 1+28+10+10)
			})

			Convey("And successive sweeps march deterministically through the rest", func() {
				_, err := b.refresh(ctx, now.Add(time.Second), snaps, 2)
				So(err, ShouldBeNil)
				So(len(b.failCache), ShouldEqual, 8)
				_, err = b.refresh(ctx, now.Add(2*time.Second), snaps, 2)
				So(err, ShouldBeNil)
				So(len(b.failCache), ShouldEqual, 10)

				Convey("Then the steady state issues zero baseline reads", func() {
					reads, err := b.refresh(ctx, now.Add(3*time.Second), snaps, 2)
					So(err, ShouldBeNil)
					So(reads, ShouldEqual, 0)
				})
			})
		})
	})

	Convey("Given a tier-2 refresher with a tiny cap for the depth spread", t, func() {
		b := newBaselineRefresher(rc, 3)
		snaps := auxSnaps(10, now)

		Convey("When the hourly depth pass starts", func() {
			_, err := b.refresh(ctx, now, snaps, 2)
			So(err, ShouldBeNil)

			Convey("Then only cap rings are read per sweep and the rest stay pending", func() {
				So(len(b.depthPending), ShouldEqual, 7)
				So(len(b.depthCache), ShouldEqual, 3)
			})

			Convey("Then the pending set drains across sweeps within the hour", func() {
				for i := 1; i <= 3; i++ {
					_, err := b.refresh(ctx, now.Add(time.Duration(i)*time.Second), snaps, 2)
					So(err, ShouldBeNil)
				}
				So(len(b.depthPending), ShouldEqual, 0)
				So(len(b.depthCache), ShouldEqual, 10)
			})
		})
	})

	Convey("Given a tier-1 refresher with the same cap", t, func() {
		b := newBaselineRefresher(rc, 28)
		snaps := auxSnaps(10, now)

		Convey("When the first sweep refreshes in tier 1", func() {
			reads, err := b.refresh(ctx, now, snaps, 1)
			So(err, ShouldBeNil)

			Convey("Then everything loads at once — pre-phase-12 behavior preserved", func() {
				So(len(b.failCache), ShouldEqual, 10)
				// 1 anchor + 10×7 fail + 10 depth + 10 seed.
				So(reads, ShouldEqual, 1+70+10+10)
			})
		})
	})
}
