package stats

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// The sweeper lease-failure log is rate limited.
//
// A Redis user without write permission never acquires the lease, so the
// acquire fails on every tick forever. This suite proves leaseTick reports
// that failure through the engine's limiter, not through a bare logf. It
// drives the engine clock, so it never sleeps. It uses the package's DB 14
// (see engine_test.go) and the write-outage client of resilience_test.go.
// ****************************************************************************

// linesMatching returns the captured lines that contain sub.
func linesMatching(lines []string, sub string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func TestSweeperLeaseLogIsRateLimited(t *testing.T) {
	Convey("Given a stats engine whose Redis refuses every write", t, func() {
		testRedis(t)
		_, insp := testClientInspector(t)
		oomClient, oomOn := newOOMClient(t)
		ctx := context.Background()

		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		var lines []string
		eng := NewEngine(Config{
			RedisClient: oomClient,
			Inspector:   insp,
			Now:         func() time.Time { return now },
			Logf: func(format string, args ...interface{}) {
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
		oomOn.Store(true)

		Convey("When the lease tick runs 13 times over a minute", func() {
			for i := 0; i < 13; i++ {
				eng.leaseTick(ctx)
				now = now.Add(5 * time.Second)
			}
			acquire := linesMatching(lines, "acquiring sweeper lease")

			Convey("Then the first line keeps its original wording", func() {
				So(len(acquire), ShouldBeGreaterThan, 0)
				So(acquire[0], ShouldEqual, "asynqmon: stats: acquiring sweeper lease: "+errOOM.Error())
			})

			Convey("Then two lines are logged instead of 13", func() {
				So(len(acquire), ShouldEqual, 2)
				So(acquire[1], ShouldContainSubstring, "13 times")
			})

			Convey("And when the writes succeed again", func() {
				oomOn.Store(false)
				eng.leaseTick(ctx)
				now = now.Add(5 * time.Second)
				eng.leaseTick(ctx)

				Convey("Then exactly one recovery line is logged", func() {
					So(len(linesMatching(lines, "recovered after 13 failures")), ShouldEqual, 1)
					So(eng.LeaseHeld(), ShouldBeTrue)
				})
			})
		})
	})
}

func TestSweeperLeaseRenewLogUsesItsOwnKey(t *testing.T) {
	Convey("Given a stats engine that holds the sweeper lease", t, func() {
		testRedis(t)
		_, insp := testClientInspector(t)
		oomClient, oomOn := newOOMClient(t)
		ctx := context.Background()

		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		var lines []string
		eng := NewEngine(Config{
			RedisClient: oomClient,
			Inspector:   insp,
			Now:         func() time.Time { return now },
			Logf: func(format string, args ...interface{}) {
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
		eng.leaseTick(ctx)
		So(eng.LeaseHeld(), ShouldBeTrue)

		Convey("When every write fails and two ticks run", func() {
			oomOn.Store(true)
			eng.leaseTick(ctx) // renewal fails, the engine drops to standby
			eng.leaseTick(ctx) // the acquire then fails too

			Convey("Then the renewal line keeps its original wording", func() {
				renew := linesMatching(lines, "renewing sweeper lease")
				So(len(renew), ShouldEqual, 1)
				So(renew[0], ShouldEqual, "asynqmon: stats: renewing sweeper lease: "+errOOM.Error())
			})

			Convey("Then the acquire failure is not masked by the renewal failure", func() {
				acquire := linesMatching(lines, "acquiring sweeper lease")
				So(len(acquire), ShouldEqual, 1)
				So(acquire[0], ShouldEqual, "asynqmon: stats: acquiring sweeper lease: "+errOOM.Error())
			})
		})
	})
}
