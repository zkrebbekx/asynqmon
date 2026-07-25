package observe

import (
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Pure unit tests — no Redis. The Redis-touching integration suite (real
// asynq server retries, cap/TTL, panic path, endpoint shapes) lives in the
// root package's observed_handlers_test.go so that exactly one test binary
// owns — and flushes — DB 4.
// ****************************************************************************

func TestKeyBuilders(t *testing.T) {
	Convey("Given the observe key builders", t, func() {
		Convey("When keys are built with the default prefix", func() {
			ak := AttemptsKey(DefaultKeyPrefix, "billing", "task-1")
			sk := SummaryKey(DefaultKeyPrefix, "billing", "task-1")

			Convey("Then the attempts key is prefix + queue + id", func() {
				So(ak, ShouldEqual, "asynqmon:obs:billing:task-1")
			})
			Convey("And the summary key inserts the sum namespace", func() {
				So(sk, ShouldEqual, "asynqmon:obs:sum:billing:task-1")
			})
		})
	})
}

func TestOptions(t *testing.T) {
	Convey("Given the middleware option set", t, func() {
		base := config{ttl: DefaultTTL, attemptCap: DefaultAttemptCap, keyPrefix: DefaultKeyPrefix}

		Convey("When no options are applied", func() {
			cfg := base
			Convey("Then the documented defaults hold", func() {
				So(cfg.attemptCap, ShouldEqual, 30)
				So(cfg.ttl.Hours(), ShouldEqual, float64(7*24))
				So(cfg.keyPrefix, ShouldEqual, "asynqmon:obs:")
			})
		})

		Convey("When valid overrides are applied", func() {
			cfg := base
			WithAttemptCap(5)(&cfg)
			WithTTL(base.ttl / 7)(&cfg)
			WithKeyPrefix("custom:")(&cfg)

			Convey("Then each bound is replaced", func() {
				So(cfg.attemptCap, ShouldEqual, 5)
				So(cfg.ttl.Hours(), ShouldEqual, float64(24))
				So(cfg.keyPrefix, ShouldEqual, "custom:")
			})
		})

		Convey("When nonsense values are applied", func() {
			cfg := base
			WithAttemptCap(0)(&cfg)
			WithAttemptCap(-3)(&cfg)
			WithTTL(0)(&cfg)
			WithKeyPrefix("")(&cfg)

			Convey("Then the defaults are kept (ignored, not clamped)", func() {
				So(cfg.attemptCap, ShouldEqual, DefaultAttemptCap)
				So(cfg.ttl, ShouldEqual, DefaultTTL)
				So(cfg.keyPrefix, ShouldEqual, DefaultKeyPrefix)
			})
		})
	})
}

func TestTruncateErr(t *testing.T) {
	Convey("Given the 500-byte error cap", t, func() {
		Convey("When the message fits", func() {
			s := strings.Repeat("a", 500)
			Convey("Then it is stored verbatim", func() {
				So(truncateErr(s), ShouldEqual, s)
			})
		})

		Convey("When the message exceeds the cap", func() {
			s := strings.Repeat("a", 501)
			out := truncateErr(s)
			Convey("Then it is cut to exactly the cap", func() {
				So(len(out), ShouldEqual, 500)
			})
		})

		Convey("When the cut lands mid-rune", func() {
			// 166 × "€" (3 bytes each) = 498 bytes, then one more lands the
			// 500-byte cut inside the final rune.
			s := strings.Repeat("€", 167)
			out := truncateErr(s)
			Convey("Then the partial rune is dropped, keeping the string valid UTF-8", func() {
				So(len(out), ShouldEqual, 498)
				So(strings.HasSuffix(out, "€"), ShouldBeTrue)
			})
		})
	})
}
