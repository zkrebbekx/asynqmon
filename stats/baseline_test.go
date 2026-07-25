package stats

import (
	"encoding/binary"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// PURE unit tests for the phase-10 baseline substrate (baseline.go) and the
// three baseline detectors (attention.go #10-12) — evaluated as pure
// functions with fabricated detectorInputs, injected clocks, no Redis.
// ****************************************************************************

func TestConsumerWindow(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given the 30m change-point consumer window", t, func() {
		Convey("Then repeated identical observations store one point", func() {
			win := appendConsumerObs(nil, t0, 6)
			win = appendConsumerObs(win, t0.Add(5*time.Second), 6)
			win = appendConsumerObs(win, t0.Add(10*time.Second), 6)
			So(win, ShouldHaveLength, 1)
			So(win[0].t, ShouldEqual, t0)
		})

		Convey("Then value changes append change-points", func() {
			win := appendConsumerObs(nil, t0, 6)
			win = appendConsumerObs(win, t0.Add(time.Minute), 2)
			win = appendConsumerObs(win, t0.Add(2*time.Minute), 6)
			So(win, ShouldHaveLength, 3)
		})

		Convey("When points age out of the window", func() {
			win := []consumerObsPoint{
				{t: t0.Add(-40 * time.Minute), n: 6},
				{t: t0.Add(-35 * time.Minute), n: 4},
				{t: t0.Add(-10 * time.Minute), n: 2},
			}
			pruned := pruneConsumerWindow(win, t0)
			Convey("Then the newest aged-out point is kept as the boundary observation (its value was current when the window began)", func() {
				So(pruned, ShouldHaveLength, 2)
				So(pruned[0].n, ShouldEqual, 4)
				So(pruned[1].n, ShouldEqual, 2)
			})
		})

		Convey("When every change is recent", func() {
			win := []consumerObsPoint{{t: t0.Add(-5 * time.Minute), n: 6}}
			Convey("Then pruning keeps it untouched", func() {
				So(pruneConsumerWindow(win, t0), ShouldHaveLength, 1)
			})
		})
	})
}

func TestDepthBaselineComputation(t *testing.T) {
	now := time.Unix(1_753_401_800, 0)
	hourP := seriesPeriodIndex(now.Unix(), rollupRing.period)

	// ring builds a rollup pending ring whose first period is `ageHours` ago
	// with `samples` consecutive hourly values of `depth` ending last hour.
	ring := func(ageHours, samples int, depth uint32) []byte {
		first := hourP - int64(ageHours)
		buf := buildSeriesInit(rollupRing, first, depth)
		last := hourP - 1
		copy(buf[8:12], encodeLastPeriod(last))
		for p := first + 1; p <= last; p++ {
			off := seriesSlotOffset(seriesSlot(p, rollupRing.slots))
			if p > last-int64(samples) {
				binary.BigEndian.PutUint32(buf[off:], depth)
			} else {
				binary.BigEndian.PutUint32(buf[off:], seriesSentinel)
			}
		}
		return buf
	}

	Convey("Given the DEPTH_ANOMALY 24h baseline", t, func() {
		Convey("Then a ring with ≥24h of history and enough samples is a valid baseline", func() {
			b := computeDepthBaseline(ring(30, 20, 250), now)
			So(b.ok, ShouldBeTrue)
			So(b.mean, ShouldAlmostEqual, 250, 0.01)
		})

		Convey("Then a young ring (<24h old) is NOT a baseline regardless of samples", func() {
			b := computeDepthBaseline(ring(10, 10, 250), now)
			So(b.ok, ShouldBeFalse)
		})

		Convey("Then a sparse ring (too few samples in the last 24h) is not a baseline", func() {
			b := computeDepthBaseline(ring(30, depthMinSamples-1, 250), now)
			So(b.ok, ShouldBeFalse)
		})

		Convey("Then an absent ring is not a baseline", func() {
			So(computeDepthBaseline(nil, now).ok, ShouldBeFalse)
		})
	})
}

// phase10Snaps builds a one-queue snapshot map for detector tests.
func phase10Snaps(q string, mut func(*QueueSnapshot)) map[string]*QueueSnapshot {
	s := &QueueSnapshot{Queue: q, Consumers: 2, RefreshedAt: time.Now()}
	if mut != nil {
		mut(s)
	}
	return map[string]*QueueSnapshot{q: s}
}

func phase10Evaluator() *attentionEvaluator {
	return newAttentionEvaluator(attentionConfig{
		pendingAgeSLO:       time.Hour,
		retryStormThreshold: 1 << 40,
		pausedLongAfter:     time.Hour,
		groupStallAfter:     time.Hour,
		orphanGrace:         30 * time.Second,
		failSpikeRatio:      4,
		failSpikeMinCount:   100,
		depthAnomalyRatio:   3,
		depthAnomalyMin:     500,
	})
}

func TestFailSpikeDetector(t *testing.T) {
	// Noon UTC: half the day elapsed, so expected = avgDaily/2.
	noon := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given the FAIL_SPIKE detector (7-day same-hour baseline, ratio 4x, min 100)", t, func() {
		ev := phase10Evaluator()

		Convey("When today's failures are 8x the prorated baseline", func() {
			// avgDaily 100 → expected by noon 50; 400 ≥ 4×50 and ≥ min 100.
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {avgDaily: 100, days: 7}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 400 }), nil, nil, det, noon)

			Convey("Then it fires with ratio, baseline and a sparkline-able series ref", func() {
				So(rep.Findings, ShouldHaveLength, 1)
				f := rep.Findings[0]
				So(f.Detector, ShouldEqual, DetectorFailSpike)
				So(f.Severity, ShouldEqual, 4)
				So(f.Value, ShouldEqual, 400)
				So(f.Sentence, ShouldContainSubstring, "8× the 7-day same-hour baseline")
				So(f.Chips, ShouldResemble, []string{"FAIL SPIKE"})
				So(f.SeriesScope, ShouldEqual, "queue:q1")
				So(f.SeriesMetric, ShouldEqual, MetricFailedDelta)
				So(f.SuggestedQuery, ShouldEqual, "state=archived queue=q1")
			})
		})

		Convey("When failures are below the ratio knob", func() {
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {avgDaily: 1000, days: 7}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 1999 }), nil, nil, det, noon)
			Convey("Then it stays silent (1999 < 4×500 expected)", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When failures exceed the ratio but not the min count", func() {
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {avgDaily: 2, days: 7}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 99 }), nil, nil, det, noon)
			Convey("Then the min-count floor holds (early-morning guard)", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When the 7-day baseline is zero failures", func() {
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {avgDaily: 0, days: 7}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 150 }), nil, nil, det, noon)
			Convey("Then any min-count-clearing failure run fires, labeled against the zero baseline", func() {
				So(rep.Findings, ShouldHaveLength, 1)
				So(rep.Findings[0].Sentence, ShouldContainSubstring, "7-day baseline of zero failures")
			})
		})

		Convey("When the queue has no baseline days at all (never existed before today)", func() {
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {days: 0}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 100000 }), nil, nil, det, noon)
			Convey("Then there is no norm to spike against — silent", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When today's count is 10x-ratio catastrophic", func() {
			det := detectorInputs{failBaselines: map[string]failBaseline{"q1": {avgDaily: 100, days: 7}}}
			rep := ev.evaluate(phase10Snaps("q1", func(s *QueueSnapshot) { s.FailedToday = 2001 }), nil, nil, det, noon)
			Convey("Then severity escalates to 5", func() {
				So(rep.Findings, ShouldHaveLength, 1)
				So(rep.Findings[0].Severity, ShouldEqual, 5)
			})
		})
	})
}

func TestDepthAnomalyDetector(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given the DEPTH_ANOMALY detector (ratio 3x vs 24h mean, min depth 500, 2-sweep debounce)", t, func() {
		ev := phase10Evaluator()
		det := detectorInputs{depthBaselines: map[string]depthBaseline{"q1": {mean: 200, samples: 24, ok: true}}}
		deep := phase10Snaps("q1", func(s *QueueSnapshot) { s.Pending = 900 })

		Convey("When depth exceeds the baseline across two consecutive sweeps", func() {
			rep1 := ev.evaluate(deep, nil, nil, det, t0)
			rep2 := ev.evaluate(deep, nil, nil, det, t0.Add(5*time.Second))

			Convey("Then the first sweep withholds (debounce) and the second fires", func() {
				So(rep1.Findings, ShouldBeEmpty)
				So(rep2.Findings, ShouldHaveLength, 1)
				f := rep2.Findings[0]
				So(f.Detector, ShouldEqual, DetectorDepthAnomaly)
				So(f.Severity, ShouldEqual, 3)
				So(f.Value, ShouldEqual, 900)
				So(f.Sentence, ShouldContainSubstring, "24h baseline")
				So(f.SeriesScope, ShouldEqual, "queue:q1")
				So(f.SeriesMetric, ShouldEqual, MetricPending)
				So(f.Since, ShouldEqual, t0.Format(time.RFC3339))
			})
		})

		Convey("When the detector is still learning (substrate <24h old)", func() {
			learning := det
			learning.depthLearningUntil = t0.Add(20 * time.Hour)
			rep1 := ev.evaluate(deep, nil, nil, learning, t0)
			rep2 := ev.evaluate(deep, nil, nil, learning, t0.Add(5*time.Second))

			Convey("Then it never fires and reports itself learning with its go-live time", func() {
				So(rep1.Findings, ShouldBeEmpty)
				So(rep2.Findings, ShouldBeEmpty)
				So(rep2.DetectorsLearning, ShouldEqual, 1)
				So(rep2.DetectorsLive, ShouldEqual, detectorsTotal-1)
				So(rep2.Learning, ShouldHaveLength, 1)
				So(rep2.Learning[0].Detector, ShouldEqual, DetectorDepthAnomaly)
				So(rep2.Learning[0].Until, ShouldEqual, t0.Add(20*time.Hour).Format(time.RFC3339))
			})
		})

		Convey("When the queue's own baseline is not valid yet (young/sparse ring)", func() {
			notOK := detectorInputs{depthBaselines: map[string]depthBaseline{"q1": {mean: 200, samples: 5, ok: false}}}
			ev.evaluate(deep, nil, nil, notOK, t0)
			rep := ev.evaluate(deep, nil, nil, notOK, t0.Add(5*time.Second))
			Convey("Then it stays silent for that queue", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When depth is anomalous by ratio but under the min-depth floor", func() {
			shallow := phase10Snaps("q1", func(s *QueueSnapshot) { s.Pending = 499 })
			tiny := detectorInputs{depthBaselines: map[string]depthBaseline{"q1": {mean: 1, samples: 24, ok: true}}}
			ev.evaluate(shallow, nil, nil, tiny, t0)
			rep := ev.evaluate(shallow, nil, nil, tiny, t0.Add(5*time.Second))
			Convey("Then the floor holds", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When a normally-empty queue (mean ~0) suddenly holds 600", func() {
			empty := detectorInputs{depthBaselines: map[string]depthBaseline{"q1": {mean: 0, samples: 24, ok: true}}}
			burst := phase10Snaps("q1", func(s *QueueSnapshot) { s.Pending = 600 })
			ev.evaluate(burst, nil, nil, empty, t0)
			rep := ev.evaluate(burst, nil, nil, empty, t0.Add(5*time.Second))
			Convey("Then it fires (judged against depth 1, not a division blowup)", func() {
				So(rep.Findings, ShouldHaveLength, 1)
			})
		})
	})
}

func TestConsumersDroppedDetector(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given the CONSUMERS_DROPPED detector (≥half the 30m peak, 2-sweep debounce)", t, func() {
		ev := phase10Evaluator()
		dropped := func(cur int) map[string]*QueueSnapshot {
			return phase10Snaps("q1", func(s *QueueSnapshot) { s.Consumers = cur })
		}
		det := func(max, cur int) detectorInputs {
			return detectorInputs{consumerDrops: map[string]consumerDropObs{
				"q1": {max: max, current: cur, covered: true},
			}}
		}

		Convey("When consumers fell 6→2 across two consecutive sweeps", func() {
			rep1 := ev.evaluate(dropped(2), nil, nil, det(6, 2), t0)
			rep2 := ev.evaluate(dropped(2), nil, nil, det(6, 2), t0.Add(5*time.Second))

			Convey("Then the first sweep withholds and the second fires with the 6→2 chip", func() {
				So(rep1.Findings, ShouldBeEmpty)
				So(rep2.Findings, ShouldHaveLength, 1)
				f := rep2.Findings[0]
				So(f.Detector, ShouldEqual, DetectorConsumersDropped)
				So(f.Severity, ShouldEqual, 4)
				So(f.Value, ShouldEqual, 2)
				So(f.Sentence, ShouldContainSubstring, "6→2 within 30m")
				So(f.Chips, ShouldResemble, []string{"CONSUMERS 6→2"})
				So(f.SeriesScope, ShouldEqual, "queue:q1")
				So(f.SeriesMetric, ShouldEqual, MetricConsumerCount)
				So(f.SuggestedQuery, ShouldEqual, "name=q1 consumers<=2")
			})
		})

		Convey("When the drop is above half (6→4)", func() {
			ev.evaluate(dropped(4), nil, nil, det(6, 4), t0)
			rep := ev.evaluate(dropped(4), nil, nil, det(6, 4), t0.Add(5*time.Second))
			Convey("Then it stays silent", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When consumers dropped to ZERO with pending work", func() {
			toZero := phase10Snaps("q1", func(s *QueueSnapshot) { s.Consumers = 0; s.Pending = 50 })
			in := det(6, 0)
			rep1 := ev.evaluate(toZero, nil, nil, in, t0)
			rep2 := ev.evaluate(toZero, nil, nil, in, t0.Add(5*time.Second))
			Convey("Then NO_CONSUMERS owns the finding — no duplicate CONSUMERS_DROPPED row", func() {
				So(findingFor(rep1, "q1", DetectorConsumersDropped), ShouldBeNil)
				So(findingFor(rep2, "q1", DetectorConsumersDropped), ShouldBeNil)
				So(findingFor(rep2, "q1", DetectorNoConsumers), ShouldNotBeNil)
			})
		})

		Convey("When the window is not yet covered (young queue)", func() {
			in := detectorInputs{consumerDrops: map[string]consumerDropObs{
				"q1": {max: 6, current: 2, covered: false},
			}}
			ev.evaluate(dropped(2), nil, nil, in, t0)
			rep := ev.evaluate(dropped(2), nil, nil, in, t0.Add(5*time.Second))
			Convey("Then it stays silent", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})

		Convey("When the detector is globally learning (first 30m of series history)", func() {
			in := det(6, 2)
			in.consumerLearningUntil = t0.Add(10 * time.Minute)
			ev.evaluate(dropped(2), nil, nil, in, t0)
			rep := ev.evaluate(dropped(2), nil, nil, in, t0.Add(5*time.Second))
			Convey("Then it never fires and the census reports it learning", func() {
				So(rep.Findings, ShouldBeEmpty)
				So(rep.DetectorsLearning, ShouldEqual, 1)
				So(rep.Learning[0].Detector, ShouldEqual, DetectorConsumersDropped)
			})
		})

		Convey("When a 1→0 idle queue winds down (max below 2)", func() {
			idle := phase10Snaps("q1", func(s *QueueSnapshot) { s.Consumers = 0 })
			in := det(1, 0)
			ev.evaluate(idle, nil, nil, in, t0)
			rep := ev.evaluate(idle, nil, nil, in, t0.Add(5*time.Second))
			Convey("Then it stays silent (a single consumer leaving is not a fleet event)", func() {
				So(rep.Findings, ShouldBeEmpty)
			})
		})
	})
}

func TestBaselineInputsAssembly(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given a refresher with an anchored series history", t, func() {
		b := newBaselineRefresher(nil)
		b.since = t0.Add(-2 * time.Hour)
		b.sinceProbed = true

		snaps := phase10Snaps("q1", func(s *QueueSnapshot) { s.Consumers = 2 })
		b.windows["q1"] = []consumerObsPoint{
			{t: t0.Add(-29 * time.Minute), n: 6},
			{t: t0.Add(-10 * time.Minute), n: 2},
		}

		Convey("When inputs are assembled 2h into the substrate's life", func() {
			det := b.inputs(t0, snaps)

			Convey("Then DEPTH_ANOMALY is still learning (needs 24h) with the anchored go-live time", func() {
				So(det.depthLearningUntil.IsZero(), ShouldBeFalse)
				So(det.depthLearningUntil, ShouldEqual, t0.Add(22*time.Hour))
			})
			Convey("Then CONSUMERS_DROPPED is already live (needs 30m)", func() {
				So(det.consumerLearningUntil.IsZero(), ShouldBeTrue)
			})
			Convey("Then the consumer window folds into max/current/covered", func() {
				obs := det.consumerDrops["q1"]
				So(obs.max, ShouldEqual, 6)
				So(obs.current, ShouldEqual, 2)
				So(obs.covered, ShouldBeTrue)
			})
		})

		Convey("When the current consumer count exceeds every window point", func() {
			snaps["q1"].Consumers = 9
			det := b.inputs(t0, snaps)
			Convey("Then max includes the live observation", func() {
				So(det.consumerDrops["q1"].max, ShouldEqual, 9)
			})
		})

		Convey("When the window's oldest point is too recent for coverage", func() {
			b.windows["q1"] = []consumerObsPoint{{t: t0.Add(-10 * time.Minute), n: 6}}
			det := b.inputs(t0, snaps)
			Convey("Then the queue is not covered", func() {
				So(det.consumerDrops["q1"].covered, ShouldBeFalse)
			})
		})
	})
}
