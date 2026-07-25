package hygiene

import (
	"strconv"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Pure unit tests: buildStorage + the retention bucket math over fake
// substrate inputs. No Redis.
// ****************************************************************************

var stNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestBuildStorage(t *testing.T) {
	Convey("Given sampled probes for two queues and one unsampled queue with tasks", t, func() {
		snaps := []*stats.QueueSnapshot{
			{Queue: "big", Pending: 100},
			{Queue: "medium", Archived: 10},
			{Queue: "small-not-sampled", Retry: 3},
			{Queue: "empty"},
		}
		probes := []storageProbe{
			{
				Queue: "big", Tasks: 100, StructBytes: 5_000, SampledAt: stNow,
				Samples: []taskSample{
					{ID: "a", State: "pending", MemBytes: 1000, MsgBytes: 900},
					{ID: "b", State: "pending", MemBytes: 3000, MsgBytes: 2900},
				},
			},
			{
				Queue: "medium", Tasks: 10, StructBytes: 800, SampledAt: stNow,
				Samples: []taskSample{
					{ID: "c", State: "archived", MemBytes: 500, MsgBytes: 400},
				},
			},
		}
		retention := []RetentionRow{
			{Queue: "big", Counts: []int64{5, 10, 0, 20, 0, 0, 65}, Total: 100},
			{Queue: "medium", Counts: []int64{0, 0, 3, 0, 0, 0, 0}, Total: 3},
		}

		Convey("When the storage report is built", func() {
			rep := buildStorage(storageInputs{snaps: snaps, probes: probes, retentionRows: retention})

			Convey("Then per-queue memory is struct bytes + avg sampled task bytes × tasks", func() {
				So(rep.Memory, ShouldHaveLength, 2)
				So(rep.Memory[0].Queue, ShouldEqual, "big") // largest estimate first
				So(rep.Memory[0].AvgTaskBytes, ShouldEqual, 2000)
				So(rep.Memory[0].EstimatedBytes, ShouldEqual, 5_000+2000*100)
				So(rep.Memory[0].SampledTasks, ShouldEqual, 2)
				So(rep.Memory[0].SampledAt, ShouldEqual, stNow)
			})

			Convey("Then queues with tasks but no probe count as 'not sampled' — never a zero row", func() {
				So(rep.NotSampledQueues, ShouldEqual, 1)
				for _, m := range rep.Memory {
					So(m.Queue, ShouldNotEqual, "small-not-sampled")
				}
			})

			Convey("Then the honest sampling method label rides on the report", func() {
				So(rep.Method, ShouldContainSubstring, "sampled")
				So(rep.Method, ShouldContainSubstring, "not sampled")
			})

			Convey("Then offenders are the sampled tasks, largest msg first", func() {
				So(len(rep.Offenders), ShouldEqual, 3)
				So(rep.Offenders[0].ID, ShouldEqual, "b")
				So(rep.Offenders[0].MsgBytes, ShouldEqual, 2900)
				So(rep.Offenders[0].State, ShouldEqual, "pending")
			})

			Convey("Then the fleet retention histogram sums the per-queue exact buckets", func() {
				So(rep.Retention.Fleet.Counts, ShouldResemble, []int64{5, 10, 3, 20, 0, 0, 65})
				So(rep.Retention.Fleet.Total, ShouldEqual, 103)
				So(rep.Retention.BucketLabels, ShouldResemble, []string{"past_due", "<1h", "1-6h", "6-24h", "1-3d", "3-7d", ">7d"})
			})

			Convey("Then the summary carries reclaim pressure: 24h horizon and past-due", func() {
				sum := storageSummary(rep)
				So(sum["sampled_queues"], ShouldEqual, 2)
				So(sum["not_sampled_queues"], ShouldEqual, 1)
				So(sum["estimated_bytes"], ShouldEqual, (5_000+2000*100)+(800+500*10))
				So(sum["completed_total"], ShouldEqual, 103)
				So(sum["reclaim_24h"], ShouldEqual, 5+10+3+20)
				So(sum["reclaim_past_due"], ShouldEqual, 5)
			})
		})
	})

	Convey("Given a probe whose task samples all missed (tasks moved mid-read)", t, func() {
		probes := []storageProbe{{Queue: "raced", Tasks: 50, StructBytes: 700, SampledAt: stNow}}

		Convey("When the report is built", func() {
			rep := buildStorage(storageInputs{probes: probes})

			Convey("Then the row keeps the exact structure bytes and claims no task estimate", func() {
				So(rep.Memory[0].SampledTasks, ShouldEqual, 0)
				So(rep.Memory[0].AvgTaskBytes, ShouldEqual, 0)
				So(rep.Memory[0].EstimatedBytes, ShouldEqual, 700)
			})
		})
	})
}

func TestRetentionBucketRanges(t *testing.T) {
	Convey("Given a fixed instant", t, func() {
		ranges := retentionBucketRanges(stNow)
		sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

		Convey("Then there is one range per bucket label", func() {
			So(len(ranges), ShouldEqual, len(retentionBucketLabels))
		})

		Convey("Then past_due covers (-inf, now] and >7d covers (now+7d, +inf)", func() {
			So(ranges[0], ShouldResemble, [2]string{"-inf", sec(stNow)})
			So(ranges[6], ShouldResemble, [2]string{"(" + sec(stNow.Add(7*24*time.Hour)), "+inf"})
		})

		Convey("Then interior buckets are half-open and contiguous (no double counting)", func() {
			// Each bucket's min is the previous bucket's max with an exclusive '('.
			for i := 1; i < len(ranges)-1; i++ {
				So(ranges[i][0], ShouldEqual, "("+ranges[i-1][1])
			}
		})
	})
}
