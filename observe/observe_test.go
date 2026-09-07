package observe

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

			Convey("Then the attempts key carries the att namespace", func() {
				// A discriminator segment on BOTH key kinds keeps the
				// namespaces disjoint: a queue named "sum" used to collide
				// an attempt LIST with a summary HASH.
				So(ak, ShouldEqual, "asynqmon:obs:att:billing:task-1")
			})
			Convey("And the summary key inserts the sum namespace", func() {
				So(sk, ShouldEqual, "asynqmon:obs:sum:billing:task-1")
			})
			Convey("And a queue literally named sum cannot collide the two", func() {
				So(AttemptsKey(DefaultKeyPrefix, "sum", "t"), ShouldNotEqual,
					SummaryKey(DefaultKeyPrefix, "", "t"))
			})
		})
	})
}

func TestOptions(t *testing.T) {
	Convey("Given the middleware option set", t, func() {
		base := defaultConfig()

		Convey("When no options are applied", func() {
			cfg := base
			Convey("Then the documented defaults hold", func() {
				So(cfg.attemptCap, ShouldEqual, 30)
				So(cfg.ttl, ShouldEqual, 24*time.Hour)
				So(cfg.keyPrefix, ShouldEqual, "asynqmon:obs:")
				So(cfg.sampleRate, ShouldEqual, 1.0)
			})
			Convey("Then only error and panic outcomes are recorded", func() {
				So(cfg.records("t1", OutcomeError), ShouldBeTrue)
				So(cfg.records("t1", OutcomePanic), ShouldBeTrue)
				So(cfg.records("t1", OutcomeOK), ShouldBeFalse)
			})
		})

		Convey("When valid overrides are applied", func() {
			cfg := base
			WithAttemptCap(5)(&cfg)
			WithTTL(time.Hour)(&cfg)
			WithKeyPrefix("custom:")(&cfg)
			WithOutcomes(OutcomeOK)(&cfg)
			WithSampling(0.25)(&cfg)

			Convey("Then each bound is replaced", func() {
				So(cfg.attemptCap, ShouldEqual, 5)
				So(cfg.ttl, ShouldEqual, time.Hour)
				So(cfg.keyPrefix, ShouldEqual, "custom:")
				So(cfg.sampleRate, ShouldEqual, 0.25)
			})
			Convey("Then the outcome set is replaced, not merged", func() {
				So(cfg.outcomes, ShouldResemble, map[Outcome]bool{OutcomeOK: true})
			})
		})

		Convey("When nonsense values are applied", func() {
			cfg := base
			WithAttemptCap(0)(&cfg)
			WithAttemptCap(-3)(&cfg)
			WithTTL(0)(&cfg)
			WithKeyPrefix("")(&cfg)
			WithOutcomes()(&cfg)
			WithSampling(0)(&cfg)
			WithSampling(-1)(&cfg)
			WithSampling(1.5)(&cfg)

			Convey("Then the defaults are kept (ignored, not clamped)", func() {
				So(cfg.attemptCap, ShouldEqual, DefaultAttemptCap)
				So(cfg.ttl, ShouldEqual, DefaultTTL)
				So(cfg.keyPrefix, ShouldEqual, DefaultKeyPrefix)
				So(cfg.sampleRate, ShouldEqual, 1.0)
				So(cfg.outcomes, ShouldResemble, map[Outcome]bool{OutcomeError: true, OutcomePanic: true})
			})
		})
	})
}

func TestOutcomeFilter(t *testing.T) {
	Convey("Given a middleware configured to record every outcome", t, func() {
		cfg := defaultConfig()
		WithOutcomes(OutcomeOK, OutcomeError, OutcomePanic)(&cfg)

		Convey("Then every outcome passes the filter", func() {
			So(cfg.records("t", OutcomeOK), ShouldBeTrue)
			So(cfg.records("t", OutcomeError), ShouldBeTrue)
			So(cfg.records("t", OutcomePanic), ShouldBeTrue)
		})
	})

	Convey("Given a middleware configured to record only panics", t, func() {
		cfg := defaultConfig()
		WithOutcomes(OutcomePanic)(&cfg)

		Convey("Then errors and successes are dropped", func() {
			So(cfg.records("t", OutcomePanic), ShouldBeTrue)
			So(cfg.records("t", OutcomeError), ShouldBeFalse)
			So(cfg.records("t", OutcomeOK), ShouldBeFalse)
		})
	})
}

func TestSampling(t *testing.T) {
	Convey("Given the per-task sampling decision", t, func() {
		ids := make([]string, 0, 2000)
		for i := 0; i < 2000; i++ {
			ids = append(ids, fmt.Sprintf("task-%d", i))
		}
		count := func(rate float64) int {
			n := 0
			for _, id := range ids {
				if Sampled(id, rate) {
					n++
				}
			}
			return n
		}

		Convey("Then rate 1 samples every task and rate 0 samples none", func() {
			So(count(1), ShouldEqual, 2000)
			So(count(0), ShouldEqual, 0)
		})

		Convey("Then rate 0.5 samples about half of the tasks", func() {
			n := count(0.5)
			So(n, ShouldBeBetween, 850, 1150)
		})

		Convey("Then rate 0.1 samples about a tenth of the tasks", func() {
			n := count(0.1)
			So(n, ShouldBeBetween, 130, 270)
		})

		Convey("Then the decision is stable for one task id", func() {
			for _, id := range ids[:50] {
				first := Sampled(id, 0.5)
				So(Sampled(id, 0.5), ShouldEqual, first)
			}
		})

		Convey("Then a task sampled at a low rate is also sampled at every higher rate", func() {
			for _, id := range ids {
				if Sampled(id, 0.1) {
					So(Sampled(id, 0.5), ShouldBeTrue)
				}
			}
		})

		Convey("Then the filter combines sampling with the outcome set", func() {
			cfg := defaultConfig()
			WithSampling(0.5)(&cfg)
			WithOutcomes(OutcomeOK, OutcomeError, OutcomePanic)(&cfg)
			sampled, dropped := 0, 0
			for _, id := range ids {
				if cfg.records(id, OutcomeError) {
					sampled++
				} else {
					dropped++
				}
			}
			So(sampled, ShouldBeBetween, 850, 1150)
			So(sampled+dropped, ShouldEqual, 2000)
		})
	})
}

func TestCountKey(t *testing.T) {
	Convey("Given the per-day counter key builder", t, func() {
		Convey("Then the key carries the count namespace and the UTC day", func() {
			So(CountKey(DefaultKeyPrefix, "2026-09-07"), ShouldEqual, "asynqmon:obs:count:2026-09-07")
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
