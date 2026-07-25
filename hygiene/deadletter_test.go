package hygiene

import (
	"strconv"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/errsig"
)

// ****************************************************************************
// Pure unit tests: buildDeadLetter + the age bucket math over fake substrate
// inputs. No Redis.
// ****************************************************************************

var dlNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestBuildDeadLetter(t *testing.T) {
	Convey("Given per-queue age buckets, an errsig index and trim metadata", t, func() {
		buckets := []deadLetterQueueBuckets{
			{Queue: "billing", Buckets: []int64{100, 400, 1500, 8000}}, // 10000 = at trim cap
			{Queue: "notif", Buckets: []int64{5, 10, 0, 0}},           // nothing over 30d
		}
		sigs := []*errsig.Signature{
			{Sig: "sig-timeout", Template: "gw timeout after ~N~s", Counts: []errsig.QueueTypeCount{
				{Queue: "billing", Type: "invoice", Archived: 6000},
				{Queue: "billing", Type: "refund", Archived: 1000},
				{Queue: "notif", Type: "push", Archived: 10},
			}},
			{Sig: "sig-conn", Template: "conn refused ~HOST~", Counts: []errsig.QueueTypeCount{
				{Queue: "billing", Type: "invoice", Archived: 2500},
			}},
		}
		meta := &errsig.Meta{TailAt: dlNow.Add(-time.Minute), TrimmedQueues: []string{"billing"}}

		Convey("When the dead-letter report is built", func() {
			rep := buildDeadLetter(deadLetterInputs{
				buckets: buckets, queuesWithArchived: 2, sigs: sigs, meta: meta,
			})

			Convey("Then queues sort by archived total and carry exact buckets", func() {
				So(rep.Queues[0].Queue, ShouldEqual, "billing")
				So(rep.Queues[0].Archived, ShouldEqual, 10000)
				So(rep.Queues[0].Buckets, ShouldResemble, []int64{100, 400, 1500, 8000})
				So(rep.Queues[0].OlderThanThreshold, ShouldEqual, 8000)
				So(rep.BucketLabels, ShouldResemble, []string{"<1d", "1-7d", "7-30d", ">30d"})
			})

			Convey("Then the 10k queue is flagged at the trim cap and its cluster counts are lower bounds", func() {
				So(rep.Queues[0].AtTrimCap, ShouldBeTrue)
				So(rep.Queues[1].AtTrimCap, ShouldBeFalse)
				for _, c := range rep.Queues[0].Clusters {
					So(c.LowerBound, ShouldBeTrue)
				}
				So(rep.Queues[1].Clusters[0].LowerBound, ShouldBeFalse)
			})

			Convey("Then clusters are the queue's top signatures with type-summed archived counts", func() {
				cl := rep.Queues[0].Clusters
				So(cl, ShouldHaveLength, 2)
				So(cl[0].Sig, ShouldEqual, "sig-timeout")
				So(cl[0].Archived, ShouldEqual, 7000) // 6000 + 1000 across types
				So(cl[1].Sig, ShouldEqual, "sig-conn")
				So(rep.ClustersIndexedAt, ShouldEqual, meta.TailAt)
			})

			Convey("Then the one-click action descriptor exists only where >30d rows exist", func() {
				act := rep.Queues[0].Action
				So(act, ShouldNotBeNil)
				So(act.Verb, ShouldEqual, "delete")
				So(act.State, ShouldEqual, "archived")
				So(act.Queue, ShouldEqual, "billing")
				So(act.Aql, ShouldEqual, "died>30d")
				So(act.Estimate, ShouldEqual, 8000)
				So(rep.Queues[1].Action, ShouldBeNil)
			})

			Convey("Then the summary aggregates the digest", func() {
				sum := deadLetterSummary(rep)
				So(sum["archived_total"], ShouldEqual, 10015)
				So(sum["older_than_30d"], ShouldEqual, 8000)
				So(sum["trim_cap_queues"], ShouldEqual, 1)
				So(sum["queues_with_archived"], ShouldEqual, 2)
			})
		})
	})

	Convey("Given no errsig index at all (indexer never ran)", t, func() {
		buckets := []deadLetterQueueBuckets{{Queue: "q", Buckets: []int64{1, 0, 0, 0}}}

		Convey("When the report is built", func() {
			rep := buildDeadLetter(deadLetterInputs{buckets: buckets, queuesWithArchived: 1})

			Convey("Then rows carry empty (never nil) cluster lists and no indexed-at stamp", func() {
				So(rep.Queues[0].Clusters, ShouldNotBeNil)
				So(rep.Queues[0].Clusters, ShouldHaveLength, 0)
				So(rep.ClustersIndexedAt.IsZero(), ShouldBeTrue)
			})
		})
	})
}

func TestDeadLetterBucketRanges(t *testing.T) {
	Convey("Given a fixed instant", t, func() {
		ranges := deadLetterBucketRanges(dlNow)
		sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

		Convey("Then there is one range per bucket label", func() {
			So(len(ranges), ShouldEqual, len(deadLetterBucketLabels))
		})

		Convey("Then '<1d' covers the newest died-at scores and '>30d' the oldest", func() {
			So(ranges[0], ShouldResemble, [2]string{"(" + sec(dlNow.Add(-24*time.Hour)), "+inf"})
			So(ranges[3], ShouldResemble, [2]string{"-inf", sec(dlNow.Add(-30 * 24 * time.Hour))})
		})

		Convey("Then adjacent buckets share exclusive/inclusive bounds (no double counting)", func() {
			// 1-7d max == <1d min without '('; 7-30d max == 1-7d min without '('.
			So("("+ranges[1][1], ShouldEqual, ranges[0][0])
			So("("+ranges[2][1], ShouldEqual, ranges[1][0])
			So("("+ranges[3][1], ShouldEqual, ranges[2][0])
		})
	})
}
