package errsig

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func TestComputeTrend(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given the trend heuristic (§3.6 badge)", t, func() {
		Convey("When a signature was first seen 5 minutes ago", func() {
			got := ComputeTrend(now.Add(-5*time.Minute), now, 40, 10, now)
			Convey("Then it is new regardless of its delta", func() {
				So(got, ShouldEqual, TrendNew)
			})
		})
		Convey("When a signature was first seen exactly at the new-window edge", func() {
			got := ComputeTrend(now.Add(-TrendNewWindow), now, 5, 5, now)
			Convey("Then it still counts as new (inclusive bound)", func() {
				So(got, ShouldEqual, TrendNew)
			})
		})
		Convey("When an old signature's count rose since the previous window", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), now, 120, 80, now)
			Convey("Then it is growing", func() {
				So(got, ShouldEqual, TrendGrowing)
			})
		})
		Convey("When an old signature's count fell since the previous window", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-2*time.Minute), 60, 80, now)
			Convey("Then it is recovering (retry tasks draining)", func() {
				So(got, ShouldEqual, TrendRecovering)
			})
		})
		Convey("When an old signature is flat and recently active", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-time.Minute), 80, 80, now)
			Convey("Then it is steady", func() {
				So(got, ShouldEqual, TrendSteady)
			})
		})
		Convey("When an old signature is flat and quiet past the quiet window", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-45*time.Minute), 80, 80, now)
			Convey("Then it is recovering (stopped occurring)", func() {
				So(got, ShouldEqual, TrendRecovering)
			})
		})
		Convey("When an old signature is flat and quiet exactly at the quiet edge", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-TrendQuietAfter), 80, 80, now)
			Convey("Then it is still steady (inclusive bound)", func() {
				So(got, ShouldEqual, TrendSteady)
			})
		})
		Convey("When the previous-window count is unknown", func() {
			Convey("And the signature was recently active", func() {
				got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-time.Minute), 80, -1, now)
				Convey("Then the delta rules are skipped and it is steady", func() {
					So(got, ShouldEqual, TrendSteady)
				})
			})
			Convey("And the signature has gone quiet", func() {
				got := ComputeTrend(now.Add(-24*time.Hour), now.Add(-2*time.Hour), 80, -1, now)
				Convey("Then it is recovering", func() {
					So(got, ShouldEqual, TrendRecovering)
				})
			})
		})
		Convey("When first_seen is missing (defensive decode path)", func() {
			got := ComputeTrend(time.Time{}, now, 10, 5, now)
			Convey("Then it never claims new and falls through to the delta", func() {
				So(got, ShouldEqual, TrendGrowing)
			})
		})
		Convey("When last_seen is missing and the delta is flat", func() {
			got := ComputeTrend(now.Add(-24*time.Hour), time.Time{}, 10, 10, now)
			Convey("Then it degrades to recovering, never fabricating activity", func() {
				So(got, ShouldEqual, TrendRecovering)
			})
		})
	})
}

func TestRollSnapshot(t *testing.T) {
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given a signature's trend snapshot lifecycle", t, func() {
		Convey("When the first observation lands", func() {
			s := &Signature{ArchivedCount: 10, PrevCount: -1}
			s.rollSnapshot(base)
			Convey("Then the window opens with no completed previous window", func() {
				So(s.SnapCount, ShouldEqual, 10)
				So(s.SnapAt, ShouldEqual, base)
				So(s.PrevCount, ShouldEqual, -1)
			})
		})
		Convey("When growth happens inside one window", func() {
			s := &Signature{ArchivedCount: 10, PrevCount: -1}
			s.rollSnapshot(base)
			s.ArchivedCount = 25
			s.rollSnapshot(base.Add(3 * time.Minute))
			Convey("Then the snapshot does not roll yet", func() {
				So(s.SnapCount, ShouldEqual, 10)
				So(s.SnapAt, ShouldEqual, base)
				So(s.PrevCount, ShouldEqual, -1)
			})
		})
		Convey("When the window completes", func() {
			s := &Signature{ArchivedCount: 10, PrevCount: -1}
			s.rollSnapshot(base)
			s.ArchivedCount = 25
			rollAt := base.Add(trendWindow)
			s.rollSnapshot(rollAt)
			Convey("Then the old window's count becomes prev and a fresh window opens", func() {
				So(s.PrevCount, ShouldEqual, 10)
				So(s.SnapCount, ShouldEqual, 25)
				So(s.SnapAt, ShouldEqual, rollAt)
			})
			Convey("And the delta then reads growing", func() {
				So(ComputeTrend(base.Add(-24*time.Hour), rollAt, s.TotalCount(), s.PrevCount, rollAt), ShouldEqual, TrendGrowing)
			})
		})
		Convey("When a dead signature keeps rolling", func() {
			s := &Signature{ArchivedCount: 25, PrevCount: -1}
			s.rollSnapshot(base)
			s.rollSnapshot(base.Add(trendWindow))
			s.rollSnapshot(base.Add(2 * trendWindow))
			Convey("Then the delta flattens instead of reading growing forever", func() {
				So(s.PrevCount, ShouldEqual, 25)
				So(s.SnapCount, ShouldEqual, 25)
			})
		})
		Convey("When retry contributions shrink between windows", func() {
			s := &Signature{
				ArchivedCount: 10,
				Counts:        []QueueTypeCount{{Queue: "q", Type: "t", Archived: 10, RetrySampled: 30}},
				PrevCount:     -1,
			}
			s.rollSnapshot(base)
			s.Counts[0].RetrySampled = 5
			s.rollSnapshot(base.Add(trendWindow))
			Convey("Then the completed window records the higher total", func() {
				So(s.PrevCount, ShouldEqual, 40)
				So(s.SnapCount, ShouldEqual, 15)
			})
			Convey("And the delta then reads recovering", func() {
				now := base.Add(trendWindow)
				So(ComputeTrend(base.Add(-24*time.Hour), now, s.TotalCount(), s.PrevCount, now), ShouldEqual, TrendRecovering)
			})
		})
	})
}
