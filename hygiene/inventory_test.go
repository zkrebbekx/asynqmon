package hygiene

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Pure unit tests: buildInventory over fake substrate inputs. No Redis — the
// gather paths are covered by the root-package integration suite (DB 7).
// ****************************************************************************

func invSnap(q string, mut ...func(*stats.QueueSnapshot)) *stats.QueueSnapshot {
	s := &stats.QueueSnapshot{Queue: q, RefreshedAt: invNow}
	for _, m := range mut {
		m(s)
	}
	return s
}

var invNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func rowFor(rep *InventoryReport, q string) *InventoryRow {
	for i := range rep.Rows {
		if rep.Rows[i].Queue == q {
			return &rep.Rows[i]
		}
	}
	return nil
}

func TestBuildInventory(t *testing.T) {
	Convey("Given a fleet of snapshots with probed movement evidence", t, func() {
		snaps := []*stats.QueueSnapshot{
			// Busy queue: active tasks right now.
			invSnap("busy", func(s *stats.QueueSnapshot) { s.Active = 3; s.Pending = 10; s.Consumers = 2 }),
			// Counter movement today, nothing in flight.
			invSnap("counters", func(s *stats.QueueSnapshot) { s.ProcessedToday = 42; s.Consumers = 1 }),
			// Pending work, nobody listening.
			invSnap("uncovered", func(s *stats.QueueSnapshot) {
				s.Pending = 7
				s.OldestPendingSince = invNow.Add(-2 * time.Hour)
			}),
			// Paused 8 days.
			invSnap("paused-old", func(s *stats.QueueSnapshot) {
				s.Paused = true
				s.PausedSince = invNow.Add(-8 * 24 * time.Hour)
				s.Consumers = 1
			}),
			// Paused 1 day — under the 7d threshold.
			invSnap("paused-new", func(s *stats.QueueSnapshot) {
				s.Paused = true
				s.PausedSince = invNow.Add(-24 * time.Hour)
				s.Consumers = 1
			}),
			// Dead candidate: empty, unconsumed, probe found nothing in 30d.
			invSnap("dead"),
			// Empty + unconsumed, but the series probe saw movement 3d ago.
			invSnap("idle-recent"),
		}
		movement := map[string]movementProbe{
			"dead":        {source: ActivityDailyCounters},
			"idle-recent": {lastAt: invNow.Add(-3 * 24 * time.Hour), source: ActivitySeries},
		}

		Convey("When the inventory report is built", func() {
			rep := buildInventory(inventoryInputs{now: invNow, snaps: snaps, movement: movement})

			Convey("Then the silent probed queue is a dead-queue candidate with its evidence source", func() {
				r := rowFor(rep, "dead")
				So(r, ShouldNotBeNil)
				So(r.DeadCandidate, ShouldBeTrue)
				So(r.DeadEvidence, ShouldEqual, ActivityDailyCounters)
				So(r.LastActivitySource, ShouldEqual, ActivityNone)
				So(r.LastActivityAt.IsZero(), ShouldBeTrue)
			})

			Convey("Then a probed queue with 30d movement is NOT a candidate and carries the series evidence", func() {
				r := rowFor(rep, "idle-recent")
				So(r.DeadCandidate, ShouldBeFalse)
				So(r.LastActivitySource, ShouldEqual, ActivitySeries)
				So(r.LastActivityAt, ShouldEqual, invNow.Add(-3*24*time.Hour))
			})

			Convey("Then activity evidence prefers active-now, then today's counters, then oldest-pending", func() {
				So(rowFor(rep, "busy").LastActivitySource, ShouldEqual, ActivityActiveNow)
				So(rowFor(rep, "counters").LastActivitySource, ShouldEqual, ActivityCountersToday)
				r := rowFor(rep, "uncovered")
				So(r.LastActivitySource, ShouldEqual, ActivityOldestPending)
				So(r.LastActivityAt, ShouldEqual, invNow.Add(-2*time.Hour))
			})

			Convey("Then zero-consumer-with-pending and paused>7d are flagged (and only them)", func() {
				So(rowFor(rep, "uncovered").ZeroConsumerPending, ShouldBeTrue)
				So(rowFor(rep, "busy").ZeroConsumerPending, ShouldBeFalse)
				So(rowFor(rep, "paused-old").PausedOver7d, ShouldBeTrue)
				So(rowFor(rep, "paused-new").PausedOver7d, ShouldBeFalse)
			})

			Convey("Then flagged rows sort first — dead candidates before zero-consumer before paused", func() {
				So(rep.Rows[0].Queue, ShouldEqual, "dead")
				So(rep.Rows[1].Queue, ShouldEqual, "uncovered")
				So(rep.Rows[2].Queue, ShouldEqual, "paused-old")
			})

			Convey("Then the summary counts the whole fleet", func() {
				sum := inventorySummary(rep)
				So(sum["queues_total"], ShouldEqual, 7)
				So(sum["dead_queue_candidates"], ShouldEqual, 1)
				So(sum["zero_consumer_pending"], ShouldEqual, 1)
				So(sum["paused_over_7d"], ShouldEqual, 1)
			})
		})
	})

	Convey("Given a queue with content that was never probed", t, func() {
		snaps := []*stats.QueueSnapshot{
			invSnap("archived-only", func(s *stats.QueueSnapshot) { s.Archived = 100 }),
		}

		Convey("When the report is built", func() {
			rep := buildInventory(inventoryInputs{now: invNow, snaps: snaps, movement: nil})

			Convey("Then its activity is honestly 'unknown', not fabricated, and it is no dead candidate", func() {
				r := rowFor(rep, "archived-only")
				So(r.LastActivitySource, ShouldEqual, ActivityUnknown)
				So(r.LastActivityAt.IsZero(), ShouldBeTrue)
				So(r.DeadCandidate, ShouldBeFalse)
			})
		})
	})

	Convey("Given a candidate whose cheap test passes but that was not probed (cap)", t, func() {
		snaps := []*stats.QueueSnapshot{invSnap("unprobed-candidate")}

		Convey("When the report is built with the capped flag", func() {
			rep := buildInventory(inventoryInputs{now: invNow, snaps: snaps, probesCapped: true})

			Convey("Then the queue stays UNflagged (under-report, never fabricate) and the cap is reported", func() {
				So(rowFor(rep, "unprobed-candidate").DeadCandidate, ShouldBeFalse)
				So(rep.ProbesCapped, ShouldBeTrue)
				So(rep.ProbeCap, ShouldEqual, inventoryProbeCap)
			})
		})
	})
}

func TestSeriesMovement(t *testing.T) {
	Convey("Given 30d rollup series with varying coverage", t, func() {
		windowStart := invNow.Add(-30 * 24 * time.Hour)
		mk := func(points []stats.SeriesPoint) *stats.Series {
			return &stats.Series{Points: points}
		}
		v := func(n int64) *int64 { return &n }

		Convey("When the ring covers the window and holds only zeros, it proves silence", func() {
			pts := make([]stats.SeriesPoint, 0, 720)
			for p := int64(0); p < 720; p++ {
				pts = append(pts, stats.SeriesPoint{T: windowStart.Unix() + p*3600, V: v(0)})
			}
			covered, last := seriesMovement(mk(pts), windowStart)
			So(covered, ShouldBeTrue)
			So(last.IsZero(), ShouldBeTrue)
		})

		Convey("When the ring covers the window and holds a spike, the spike time is the movement", func() {
			spikeAt := invNow.Add(-5 * 24 * time.Hour).Truncate(time.Hour)
			pts := []stats.SeriesPoint{
				{T: windowStart.Unix(), V: v(0)},
				{T: spikeAt.Unix(), V: v(7)},
				{T: invNow.Truncate(time.Hour).Unix(), V: v(0)},
			}
			covered, last := seriesMovement(mk(pts), windowStart)
			So(covered, ShouldBeTrue)
			So(last.Unix(), ShouldEqual, spikeAt.Unix())
		})

		Convey("When the ring only reaches back 2 days it cannot prove 30d of silence", func() {
			pts := []stats.SeriesPoint{
				{T: invNow.Add(-2 * 24 * time.Hour).Unix(), V: v(0)},
			}
			covered, _ := seriesMovement(mk(pts), windowStart)
			So(covered, ShouldBeFalse)
		})

		Convey("When the ring is empty (all null) it is not coverage", func() {
			covered, last := seriesMovement(mk([]stats.SeriesPoint{{T: windowStart.Unix(), V: nil}}), windowStart)
			So(covered, ShouldBeFalse)
			So(last.IsZero(), ShouldBeTrue)
		})
	})
}
