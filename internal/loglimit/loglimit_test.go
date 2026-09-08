package loglimit

import (
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Unit tests for the repeated-error log limiter. Every test drives an
// injected clock, so no test sleeps.
// ****************************************************************************

func TestLimiterMessage(t *testing.T) {
	Convey("Given a limiter with an injected clock", t, func() {
		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		l := New(func() time.Time { return now })
		boom := errors.New("OOM command not allowed")

		Convey("When the same error repeats within a minute", func() {
			first, ok1 := l.Message("sweep", boom)
			now = now.Add(30 * time.Second)
			_, ok2 := l.Message("sweep", boom)
			now = now.Add(1 * time.Second)
			_, ok3 := l.Message("sweep", boom)

			Convey("Then only the first occurrence is logged", func() {
				So(ok1, ShouldBeTrue)
				So(first, ShouldContainSubstring, "OOM command not allowed")
				So(ok2, ShouldBeFalse)
				So(ok3, ShouldBeFalse)
			})

			Convey("And the next line comes after a minute, with the count", func() {
				now = now.Add(time.Minute)
				msg, ok := l.Message("sweep", boom)
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "4 times")
			})

			Convey("And a recovery is logged exactly once", func() {
				msg, ok := l.Message("sweep", nil)
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "recovered")
				_, ok = l.Message("sweep", nil)
				So(ok, ShouldBeFalse)
			})
		})

		Convey("When a different error arrives", func() {
			l.Message("sweep", boom)
			msg, ok := l.Message("sweep", fmt.Errorf("connection refused"))

			Convey("Then it is logged immediately, not rate-limited", func() {
				So(ok, ShouldBeTrue)
				So(msg, ShouldContainSubstring, "connection refused")
			})
		})

		Convey("When two keys fail", func() {
			_, ok1 := l.Message("series", boom)
			_, ok2 := l.Message("schedulers", boom)

			Convey("Then each key gets its own first line", func() {
				So(ok1, ShouldBeTrue)
				So(ok2, ShouldBeTrue)
			})
		})
	})
}

func TestLimiterReport(t *testing.T) {
	Convey("Given a limiter reporting through a captured logger", t, func() {
		now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		l := New(func() time.Time { return now })
		var lines []string
		logf := func(format string, args ...interface{}) {
			lines = append(lines, fmt.Sprintf(format, args...))
		}
		denied := errors.New("NOPERM this user has no permissions to run the 'set' command")
		prefix := "asynqmon: stats: acquiring sweeper lease"

		Convey("When the first failure arrives", func() {
			l.Report(logf, "lease-acquire", prefix, denied)

			Convey("Then the line keeps the original wording", func() {
				So(len(lines), ShouldEqual, 1)
				So(lines[0], ShouldEqual, prefix+": "+denied.Error())
			})
		})

		Convey("When the same failure repeats on every 5s tick over a minute", func() {
			for i := 0; i < 13; i++ {
				l.Report(logf, "lease-acquire", prefix, denied)
				now = now.Add(5 * time.Second)
			}

			Convey("Then only the first line and one repeat line are written", func() {
				So(len(lines), ShouldEqual, 2)
				So(lines[0], ShouldEqual, prefix+": "+denied.Error())
				So(lines[1], ShouldContainSubstring, "acquiring sweeper lease")
				So(lines[1], ShouldContainSubstring, "13 times")
			})

			Convey("And a success writes exactly one recovery line", func() {
				l.Report(logf, "lease-acquire", prefix, nil)
				l.Report(logf, "lease-acquire", prefix, nil)
				So(len(lines), ShouldEqual, 3)
				So(lines[2], ShouldContainSubstring, "recovered after 13 failures")
			})
		})

		Convey("When an acquire failure and a renew failure interleave", func() {
			renewPrefix := "asynqmon: stats: renewing sweeper lease"
			l.Report(logf, "lease-acquire", prefix, denied)
			l.Report(logf, "lease-renew", renewPrefix, denied)
			l.Report(logf, "lease-acquire", prefix, denied)
			l.Report(logf, "lease-renew", renewPrefix, denied)

			Convey("Then each key logs its own first line and neither masks the other", func() {
				So(len(lines), ShouldEqual, 2)
				So(lines[0], ShouldContainSubstring, "acquiring sweeper lease")
				So(lines[1], ShouldContainSubstring, "renewing sweeper lease")
			})
		})

		Convey("When a transient error follows a different failure", func() {
			l.Report(logf, "lease-acquire", prefix, denied)
			l.Report(logf, "lease-acquire", prefix, errors.New("i/o timeout"))

			Convey("Then the new message is logged at once", func() {
				So(len(lines), ShouldEqual, 2)
				So(lines[1], ShouldEqual, prefix+": i/o timeout")
			})
		})

		Convey("When a success arrives with no failure before it", func() {
			l.Report(logf, "lease-acquire", prefix, nil)

			Convey("Then nothing is logged", func() {
				So(lines, ShouldBeEmpty)
			})
		})
	})
}
