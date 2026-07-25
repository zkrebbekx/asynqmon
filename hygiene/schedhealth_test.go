package hygiene

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Pure unit tests: buildSchedulerHealth over fake substrate inputs, plus the
// config/store validation logic. No Redis.
// ****************************************************************************

var shNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestBuildSchedulerHealth(t *testing.T) {
	Convey("Given live entries, snapshots and a consumer map", t, func() {
		live := []liveSchedEntry{
			{stableKey: "k-nightly", entry: stats.SchedulerEntryData{
				TaskType: "report:nightly", Spec: "0 2 * * *", Queue: "reports", RetentionSec: 86400,
			}},
			{stableKey: "k-noretention", entry: stats.SchedulerEntryData{
				TaskType: "cleanup:tmp", Spec: "@every 1h", Queue: "maintenance", RetentionSec: 0,
			}},
			{stableKey: "k-orphan-target", entry: stats.SchedulerEntryData{
				TaskType: "sync:ledger", Spec: "@every 5m", Queue: "ledger", RetentionSec: 3600,
			}},
		}
		snapshots := []*stats.SchedulerSnapshot{
			// Live counterpart present — not gone.
			{StableKey: "k-nightly", LastSeen: shNow.Add(-5 * time.Second),
				Entry: stats.SchedulerEntryData{TaskType: "report:nightly", Queue: "reports"}},
			// Dead scheduler: last seen 2h ago, no live counterpart.
			{StableKey: "k-dead", LastSeen: shNow.Add(-2 * time.Hour),
				Entry: stats.SchedulerEntryData{TaskType: "digest:weekly", Spec: "0 6 * * 0", Queue: "email", RetentionSec: 0}},
			// Recently restarted: silent for only 10s — within the gone grace.
			{StableKey: "k-flap", LastSeen: shNow.Add(-10 * time.Second),
				Entry: stats.SchedulerEntryData{TaskType: "sync:fast", Queue: "sync"}},
		}
		consumers := map[string]int{"reports": 2, "maintenance": 1, "email": 1}

		Convey("When the scheduler-health report is built", func() {
			rep := buildSchedulerHealth(schedHealthInputs{
				now: shNow, goneAfter: stats.DefaultSchedulerGoneAfter,
				live: live, snapshots: snapshots, consumers: consumers,
			})

			Convey("Then only the snapshot silent past the threshold is GONE", func() {
				So(rep.Gone, ShouldHaveLength, 1)
				So(rep.Gone[0].TaskType, ShouldEqual, "digest:weekly")
				So(rep.Gone[0].LastSeenAt, ShouldEqual, shNow.Add(-2*time.Hour))
			})

			Convey("Then retention-less LIVE entries are flagged (a gone entry is its own finding)", func() {
				So(rep.RetentionLess, ShouldHaveLength, 1)
				So(rep.RetentionLess[0].TaskType, ShouldEqual, "cleanup:tmp")
				So(rep.RetentionLess[0].RetentionSeconds, ShouldEqual, 0)
			})

			Convey("Then entries targeting zero-consumer queues are flagged with the consumer count", func() {
				So(rep.ZeroConsumerTargets, ShouldHaveLength, 1)
				So(rep.ZeroConsumerTargets[0].TaskType, ShouldEqual, "sync:ledger")
				So(rep.ZeroConsumerTargets[0].Queue, ShouldEqual, "ledger")
				So(rep.ZeroConsumerTargets[0].Consumers, ShouldEqual, 0)
			})

			Convey("Then totals and the summary line up", func() {
				So(rep.LiveEntries, ShouldEqual, 3)
				So(rep.Snapshots, ShouldEqual, 3)
				sum := schedulerHealthSummary(rep)
				So(sum["gone"], ShouldEqual, 1)
				So(sum["retention_less"], ShouldEqual, 1)
				So(sum["zero_consumer_targets"], ShouldEqual, 1)
				So(sum["live_entries"], ShouldEqual, 3)
			})
		})
	})

	Convey("Given no schedulers at all", t, func() {
		Convey("When the report is built", func() {
			rep := buildSchedulerHealth(schedHealthInputs{now: shNow, goneAfter: stats.DefaultSchedulerGoneAfter})

			Convey("Then every list is empty but present (frontend renders [] not null)", func() {
				So(rep.Gone, ShouldNotBeNil)
				So(rep.RetentionLess, ShouldNotBeNil)
				So(rep.ZeroConsumerTargets, ShouldNotBeNil)
			})
		})
	})
}

func TestKindsAndConfig(t *testing.T) {
	Convey("Given the report-kind catalog", t, func() {
		Convey("Then all four §3.10 kinds exist in display order with titles", func() {
			So(Kinds(), ShouldResemble, []string{KindInventory, KindStorage, KindDeadLetter, KindSchedulerHealth})
			for _, k := range Kinds() {
				So(ValidKind(k), ShouldBeTrue)
				So(Title(k), ShouldNotBeBlank)
			}
			So(ValidKind("nonsense"), ShouldBeFalse)
		})

		Convey("Then defaults are DISABLED (§3.10 empty state) at the mockup cadences", func() {
			So(DefaultConfig(KindInventory), ShouldResemble, KindConfig{Enabled: false, IntervalSeconds: 7 * 24 * 3600})
			So(DefaultConfig(KindStorage), ShouldResemble, KindConfig{Enabled: false, IntervalSeconds: 30 * 24 * 3600})
			So(DefaultConfig(KindDeadLetter), ShouldResemble, KindConfig{Enabled: false, IntervalSeconds: 7 * 24 * 3600})
			So(DefaultConfig(KindSchedulerHealth), ShouldResemble, KindConfig{Enabled: false, IntervalSeconds: 24 * 3600})
		})

		Convey("Then interval validation enforces the 1m..90d bounds", func() {
			So(ValidateConfig(KindConfig{IntervalSeconds: 59}), ShouldNotBeNil)
			So(ValidateConfig(KindConfig{IntervalSeconds: 60}), ShouldBeNil)
			So(ValidateConfig(KindConfig{IntervalSeconds: MaxIntervalSeconds}), ShouldBeNil)
			So(ValidateConfig(KindConfig{IntervalSeconds: MaxIntervalSeconds + 1}), ShouldNotBeNil)
		})
	})
}
