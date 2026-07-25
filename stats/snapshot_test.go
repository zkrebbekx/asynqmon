package stats

import (
	"fmt"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// stringify simulates the Redis round trip: HSET stores every hash value as
// a string, HGETALL returns map[string]string.
func stringify(h map[string]interface{}) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func TestSnapshotHashRoundTrip(t *testing.T) {
	Convey("Given a fully populated queue snapshot", t, func() {
		now := time.Now()
		s := &QueueSnapshot{
			Queue:              "email",
			Pending:            10,
			Active:             2,
			Scheduled:          3,
			Retry:              4,
			Archived:           5,
			Completed:          6,
			Groups:             1,
			Paused:             true,
			PausedSince:        now.Add(-time.Hour),
			ProcessedToday:     100,
			FailedToday:        7,
			OrphanCandidates:   1,
			NextRetryAt:        now.Add(time.Minute),
			PastDueScheduled:   2,
			OldestPendingSince: now.Add(-30 * time.Minute),
			Consumers:          3,
			RefreshedAt:        now,
		}

		Convey("When encoded to a hash and decoded back", func() {
			got := queueSnapshotFromHash(stringify(s.toHash()))

			Convey("Then every field survives (times to nanosecond identity)", func() {
				So(got.Queue, ShouldEqual, s.Queue)
				So(got.Pending, ShouldEqual, s.Pending)
				So(got.Active, ShouldEqual, s.Active)
				So(got.Scheduled, ShouldEqual, s.Scheduled)
				So(got.Retry, ShouldEqual, s.Retry)
				So(got.Archived, ShouldEqual, s.Archived)
				So(got.Completed, ShouldEqual, s.Completed)
				So(got.Groups, ShouldEqual, s.Groups)
				So(got.Paused, ShouldEqual, s.Paused)
				So(got.PausedSince.UnixNano(), ShouldEqual, s.PausedSince.UnixNano())
				So(got.ProcessedToday, ShouldEqual, s.ProcessedToday)
				So(got.FailedToday, ShouldEqual, s.FailedToday)
				So(got.OrphanCandidates, ShouldEqual, s.OrphanCandidates)
				So(got.NextRetryAt.UnixNano(), ShouldEqual, s.NextRetryAt.UnixNano())
				So(got.PastDueScheduled, ShouldEqual, s.PastDueScheduled)
				So(got.OldestPendingSince.UnixNano(), ShouldEqual, s.OldestPendingSince.UnixNano())
				So(got.Consumers, ShouldEqual, s.Consumers)
				So(got.RefreshedAt.UnixNano(), ShouldEqual, s.RefreshedAt.UnixNano())
			})
		})

		Convey("When zero times are encoded", func() {
			empty := &QueueSnapshot{Queue: "bare"}
			got := queueSnapshotFromHash(stringify(empty.toHash()))
			Convey("Then they decode back to zero times, not 1970", func() {
				So(got.PausedSince.IsZero(), ShouldBeTrue)
				So(got.NextRetryAt.IsZero(), ShouldBeTrue)
				So(got.OldestPendingSince.IsZero(), ShouldBeTrue)
			})
		})
	})
}

func TestFleetSnapshotHashRoundTrip(t *testing.T) {
	Convey("Given a fully populated fleet snapshot", t, func() {
		now := time.Now()
		f := &FleetSnapshot{
			QueuesTotal:        12,
			PausedQueues:       2,
			ZeroConsumerQueues: 3,
			Pending:            100,
			Active:             5,
			Scheduled:          8,
			Retry:              9,
			Archived:           10,
			Completed:          11,
			Groups:             4,
			ProcessedToday:     1000,
			FailedToday:        50,
			OrphanCandidates:   1,
			PastDueScheduled:   2,
			Servers:            3,
			WorkersTotal:       30,
			WorkersBusy:        12,
			RefreshedAt:        now,
		}

		Convey("When encoded to a hash and decoded back", func() {
			got := fleetSnapshotFromHash(stringify(f.toHash()))
			Convey("Then every field survives", func() {
				So(got.QueuesTotal, ShouldEqual, f.QueuesTotal)
				So(got.PausedQueues, ShouldEqual, f.PausedQueues)
				So(got.ZeroConsumerQueues, ShouldEqual, f.ZeroConsumerQueues)
				So(got.Pending, ShouldEqual, f.Pending)
				So(got.Active, ShouldEqual, f.Active)
				So(got.Scheduled, ShouldEqual, f.Scheduled)
				So(got.Retry, ShouldEqual, f.Retry)
				So(got.Archived, ShouldEqual, f.Archived)
				So(got.Completed, ShouldEqual, f.Completed)
				So(got.Groups, ShouldEqual, f.Groups)
				So(got.ProcessedToday, ShouldEqual, f.ProcessedToday)
				So(got.FailedToday, ShouldEqual, f.FailedToday)
				So(got.OrphanCandidates, ShouldEqual, f.OrphanCandidates)
				So(got.PastDueScheduled, ShouldEqual, f.PastDueScheduled)
				So(got.Servers, ShouldEqual, f.Servers)
				So(got.WorkersTotal, ShouldEqual, f.WorkersTotal)
				So(got.WorkersBusy, ShouldEqual, f.WorkersBusy)
				So(got.RefreshedAt.UnixNano(), ShouldEqual, f.RefreshedAt.UnixNano())
			})
			Convey("Then the derived error rate is preserved through the trip", func() {
				So(got.ErrorRate(), ShouldEqual, 0.05)
			})
		})
	})
}
