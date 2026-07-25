package stats

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func TestSortSnapshots(t *testing.T) {
	Convey("Given a set of queue snapshots", t, func() {
		now := time.Now()
		a := &QueueSnapshot{Queue: "alpha", Pending: 10, Consumers: 2, ProcessedToday: 100, FailedToday: 50}
		b := &QueueSnapshot{Queue: "beta", Pending: 30, Consumers: 0, ProcessedToday: 200, FailedToday: 2,
			OldestPendingSince: now.Add(-time.Hour)}
		c := &QueueSnapshot{Queue: "gamma", Pending: 20, Consumers: 1, ProcessedToday: 0, FailedToday: 0,
			OldestPendingSince: now.Add(-time.Minute)}

		Convey("When sorting by name ascending", func() {
			s := []*QueueSnapshot{c, a, b}
			SortSnapshots(s, SortName, false, now)
			Convey("Then rows come back in lexicographic order", func() {
				So(s[0].Queue, ShouldEqual, "alpha")
				So(s[1].Queue, ShouldEqual, "beta")
				So(s[2].Queue, ShouldEqual, "gamma")
			})
		})

		Convey("When sorting by pending descending", func() {
			s := []*QueueSnapshot{a, b, c}
			SortSnapshots(s, SortPending, true, now)
			Convey("Then the deepest queue is first", func() {
				So(s[0].Queue, ShouldEqual, "beta")
				So(s[1].Queue, ShouldEqual, "gamma")
				So(s[2].Queue, ShouldEqual, "alpha")
			})
		})

		Convey("When sorting by oldest_pending_age descending", func() {
			s := []*QueueSnapshot{a, c, b}
			SortSnapshots(s, SortOldestPendingAge, true, now)
			Convey("Then the longest-waiting queue is first and no-pending queues (age 0) are last", func() {
				So(s[0].Queue, ShouldEqual, "beta")
				So(s[1].Queue, ShouldEqual, "gamma")
				So(s[2].Queue, ShouldEqual, "alpha")
			})
		})

		Convey("When sorting by latency", func() {
			s := []*QueueSnapshot{a, c, b}
			SortSnapshots(s, SortLatency, true, now)
			Convey("Then latency orders identically to oldest_pending_age (it is the same metric)", func() {
				So(s[0].Queue, ShouldEqual, "beta")
			})
		})

		Convey("When sorting by error_rate descending", func() {
			s := []*QueueSnapshot{b, c, a}
			SortSnapshots(s, SortErrorRate, true, now)
			Convey("Then rate — not raw failure count — decides the order, and zero-processed queues rank as 0", func() {
				So(s[0].Queue, ShouldEqual, "alpha") // 50%
				So(s[1].Queue, ShouldEqual, "beta")  // 1%
				So(s[2].Queue, ShouldEqual, "gamma") // no data -> 0
			})
		})

		Convey("When two rows tie on the sort value", func() {
			d := &QueueSnapshot{Queue: "delta", Pending: 10}
			e := &QueueSnapshot{Queue: "acme", Pending: 10}
			s := []*QueueSnapshot{d, e}
			SortSnapshots(s, SortPending, true, now)
			Convey("Then the tie breaks by name ascending for stable pagination", func() {
				So(s[0].Queue, ShouldEqual, "acme")
				So(s[1].Queue, ShouldEqual, "delta")
			})
		})
	})
}

func TestParseSortColumn(t *testing.T) {
	Convey("Given HTTP sort parameters", t, func() {
		Convey("When the parameter is empty", func() {
			col, err := ParseSortColumn("")
			Convey("Then it defaults to name", func() {
				So(err, ShouldBeNil)
				So(col, ShouldEqual, SortName)
			})
		})
		Convey("When the parameter names a supported column", func() {
			for _, s := range []string{"name", "pending", "active", "scheduled", "retry", "archived",
				"completed", "oldest_pending_age", "latency", "processed_today", "failed_today",
				"error_rate", "consumers"} {
				_, err := ParseSortColumn(s)
				So(err, ShouldBeNil)
			}
		})
		Convey("When the parameter is unknown", func() {
			_, err := ParseSortColumn("memory")
			Convey("Then it is rejected rather than silently sorting by nothing", func() {
				So(err, ShouldNotBeNil)
			})
		})
	})
}

func TestParseFilter(t *testing.T) {
	Convey("Given v1 filter strings", t, func() {
		now := time.Now()
		snap := &QueueSnapshot{
			Queue:              "email-prod",
			Pending:            500,
			Retry:              3,
			Consumers:          0,
			Paused:             true,
			ProcessedToday:     100,
			FailedToday:        20,
			OldestPendingSince: now.Add(-45 * time.Minute),
		}

		Convey("When the filter is empty", func() {
			f := ParseFilter("")
			Convey("Then it matches everything and ignores nothing", func() {
				So(f.Empty(), ShouldBeTrue)
				So(f.Ignored, ShouldBeEmpty)
				So(f.Match(snap, now), ShouldBeTrue)
			})
		})

		Convey("When a bare word is given", func() {
			Convey("Then it substring-matches the queue name case-insensitively", func() {
				So(ParseFilter("EMAIL").Match(snap, now), ShouldBeTrue)
				So(ParseFilter("payments").Match(snap, now), ShouldBeFalse)
			})
		})

		Convey("When name~ and name= are given", func() {
			So(ParseFilter("name~prod").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("name=email-prod").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("name=email").Match(snap, now), ShouldBeFalse)
		})

		Convey("When numeric comparisons are given", func() {
			So(ParseFilter("pending>100").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("pending<100").Match(snap, now), ShouldBeFalse)
			So(ParseFilter("pending>=500").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("pending<=499").Match(snap, now), ShouldBeFalse)
			So(ParseFilter("retry=3").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("retry!=3").Match(snap, now), ShouldBeFalse)
			So(ParseFilter("consumers=0").Match(snap, now), ShouldBeTrue)
		})

		Convey("When error_rate is compared", func() {
			Convey("Then both fractions and percent values work", func() {
				So(ParseFilter("error_rate>0.1").Match(snap, now), ShouldBeTrue)
				So(ParseFilter("error_rate>15%").Match(snap, now), ShouldBeTrue)
				So(ParseFilter("error_rate>25%").Match(snap, now), ShouldBeFalse)
			})
		})

		Convey("When age fields are compared", func() {
			Convey("Then duration values and bare seconds both work, on both aliases", func() {
				So(ParseFilter("oldest_pending_age>30m").Match(snap, now), ShouldBeTrue)
				So(ParseFilter("oldest_pending_age>1h").Match(snap, now), ShouldBeFalse)
				So(ParseFilter("latency>1800").Match(snap, now), ShouldBeTrue) // bare number = seconds
			})
		})

		Convey("When paused is filtered", func() {
			So(ParseFilter("paused=true").Match(snap, now), ShouldBeTrue)
			So(ParseFilter("paused=false").Match(snap, now), ShouldBeFalse)
			So(ParseFilter("paused!=false").Match(snap, now), ShouldBeTrue)
		})

		Convey("When multiple tokens are given", func() {
			Convey("Then every token must match (AND)", func() {
				So(ParseFilter("email pending>100 consumers=0").Match(snap, now), ShouldBeTrue)
				So(ParseFilter("email pending>1000").Match(snap, now), ShouldBeFalse)
			})
		})

		Convey("When tokens are malformed or reference unknown fields", func() {
			f := ParseFilter("bogus_field>5 pending>x pending>100 =broken")
			Convey("Then parsing is lenient: bad tokens land in Ignored, good ones still filter", func() {
				So(f.Ignored, ShouldResemble, []string{"bogus_field>5", "pending>x", "=broken"})
				So(f.Match(snap, now), ShouldBeTrue) // pending>100 survived
			})
			Convey("Then an operator-shaped token is never demoted to a name search", func() {
				// "pending>x" must not silently become substring("pending>x").
				So(ParseFilter("pending>x").Empty(), ShouldBeTrue)
				So(ParseFilter("pending>x").Ignored, ShouldHaveLength, 1)
			})
		})
	})
}
