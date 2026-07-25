package stats

import (
	"encoding/binary"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// PURE unit tests for the §5.8 series substrate: codec, slot math, gap
// ranges, window decoding and the counter-delta rules. No Redis, no wall
// clock — every instant is constructed. (The end-to-end sweep→SETRANGE→read
// path is integration-tested in the root package against DB 8.)
// ****************************************************************************

func TestSeriesSlotMath(t *testing.T) {
	Convey("Given the §5.8 ring geometry", t, func() {
		Convey("Then period index is unix/period", func() {
			So(seriesPeriodIndex(0, 30), ShouldEqual, 0)
			So(seriesPeriodIndex(29, 30), ShouldEqual, 0)
			So(seriesPeriodIndex(30, 30), ShouldEqual, 1)
			So(seriesPeriodIndex(1_753_400_000, 3600), ShouldEqual, 1_753_400_000/3600)
		})

		Convey("Then slot index is period mod N (wrapping the ring)", func() {
			So(seriesSlot(0, 360), ShouldEqual, 0)
			So(seriesSlot(359, 360), ShouldEqual, 359)
			So(seriesSlot(360, 360), ShouldEqual, 0)
			So(seriesSlot(725, 720), ShouldEqual, 5)
		})

		Convey("Then slot byte offsets sit after the 12-byte header", func() {
			So(seriesSlotOffset(0), ShouldEqual, 12)
			So(seriesSlotOffset(1), ShouldEqual, 16)
			So(seriesSlotOffset(359), ShouldEqual, 12+359*4)
		})

		Convey("Then the ring byte lengths match the contract's ~1.4KB claim", func() {
			So(hotRing.byteLen(), ShouldEqual, 12+360*4)    // 1,452
			So(rollupRing.byteLen(), ShouldEqual, 12+720*4) // 2,892
		})
	})
}

func TestSeriesValueClamp(t *testing.T) {
	Convey("Given the sentinel encoding", t, func() {
		Convey("Then values clamp into [0, 0xFFFFFFFE] so no sample aliases the sentinel", func() {
			So(clampSeriesValue(-5), ShouldEqual, uint32(0))
			So(clampSeriesValue(0), ShouldEqual, uint32(0))
			So(clampSeriesValue(42), ShouldEqual, uint32(42))
			So(clampSeriesValue(seriesMaxValue), ShouldEqual, uint32(0xFFFFFFFE))
			So(clampSeriesValue(seriesMaxValue+1), ShouldEqual, uint32(0xFFFFFFFE))
			So(clampSeriesValue(1<<40), ShouldEqual, uint32(0xFFFFFFFE))
		})
	})
}

func TestSeriesHeaderCodec(t *testing.T) {
	Convey("Given the 12-byte series header", t, func() {
		Convey("When encoded and decoded", func() {
			h := decodeSeriesHeader(encodeSeriesHeader(1234, 5678))
			Convey("Then first/last round-trip", func() {
				So(h.ok, ShouldBeTrue)
				So(h.first, ShouldEqual, 1234)
				So(h.last, ShouldEqual, 5678)
			})
		})

		Convey("Then junk, short and empty buffers decode as not-a-series", func() {
			So(decodeSeriesHeader(nil).ok, ShouldBeFalse)
			So(decodeSeriesHeader([]byte("short")).ok, ShouldBeFalse)
			So(decodeSeriesHeader([]byte("XXxx00000000")).ok, ShouldBeFalse)
			// Wrong version byte is rejected too.
			bad := encodeSeriesHeader(1, 2)
			bad[2] = 0x7F
			So(decodeSeriesHeader(bad).ok, ShouldBeFalse)
		})
	})
}

func TestSeriesInitBuffer(t *testing.T) {
	Convey("Given a first flush of period P with value 7 into the hot ring", t, func() {
		p := int64(1000) // slot 1000 % 360 = 280
		buf := buildSeriesInit(hotRing, p, 7)

		Convey("Then the buffer is full ring length", func() {
			So(len(buf), ShouldEqual, hotRing.byteLen())
		})
		Convey("Then the header stamps first = last = P", func() {
			h := decodeSeriesHeader(buf)
			So(h.ok, ShouldBeTrue)
			So(h.first, ShouldEqual, p)
			So(h.last, ShouldEqual, p)
		})
		Convey("Then P's slot holds the value and every other slot is the sentinel", func() {
			slot := seriesSlot(p, hotRing.slots)
			for i := 0; i < hotRing.slots; i++ {
				off := seriesSlotOffset(i)
				v := binary.BigEndian.Uint32(buf[off : off+4])
				if i == slot {
					So(v, ShouldEqual, uint32(7))
				} else if v != seriesSentinel {
					// Assert loudly only on failure — 359 passing So()s per
					// run would drown the report.
					So(v, ShouldEqual, seriesSentinel)
				}
			}
		})
	})
}

func TestSeriesGapRanges(t *testing.T) {
	Convey("Given the sentinel gap-fill computation", t, func() {
		Convey("Then consecutive flushes need no fill", func() {
			So(seriesGapRanges(hotRing, 100, 101), ShouldBeNil)
		})
		Convey("Then a one-slot gap fills exactly that slot", func() {
			grs := seriesGapRanges(hotRing, 100, 102) // period 101 skipped; slot 101
			So(grs, ShouldResemble, []byteRange{{offset: seriesSlotOffset(101), length: 4}})
		})
		Convey("Then a gap crossing the ring end splits into two ranges", func() {
			// lastWritten period 358, next flush 362: skipped 359,360,361 →
			// slots 359, 0, 1.
			grs := seriesGapRanges(hotRing, 358, 362)
			So(grs, ShouldResemble, []byteRange{
				{offset: seriesSlotOffset(359), length: 4},
				{offset: seriesSlotOffset(0), length: 8},
			})
		})
		Convey("Then a gap of a full ring (or more) rewrites every slot once", func() {
			grs := seriesGapRanges(hotRing, 100, 100+361)
			So(grs, ShouldResemble, []byteRange{{offset: 12, length: 360 * 4}})
			grs = seriesGapRanges(hotRing, 100, 100+5000)
			So(grs, ShouldResemble, []byteRange{{offset: 12, length: 360 * 4}})
		})
	})
}

func TestSeriesPointDecoding(t *testing.T) {
	Convey("Given a ring holding periods 998..1000 (=7,sentinel,9)", t, func() {
		buf := buildSeriesInit(hotRing, 998, 7)
		// Manual writer steps for periods 999 (skipped → sentinel via gap
		// fill semantics) and 1000 (=9), mirroring the sampler's SETRANGEs.
		copy(buf[8:12], encodeLastPeriod(1000))
		binary.BigEndian.PutUint32(buf[seriesSlotOffset(seriesSlot(999, hotRing.slots)):], seriesSentinel)
		binary.BigEndian.PutUint32(buf[seriesSlotOffset(seriesSlot(1000, hotRing.slots)):], 9)
		h := decodeSeriesHeader(buf)

		read := func(p int64) *int64 { return decodeSeriesPoint(buf, h, hotRing, p) }

		Convey("Then written periods decode to their values", func() {
			So(*read(998), ShouldEqual, 7)
			So(*read(1000), ShouldEqual, 9)
		})
		Convey("Then the skipped period decodes to no-sample", func() {
			So(read(999), ShouldBeNil)
		})
		Convey("Then periods after last are no-sample (the accumulating slot is never served)", func() {
			So(read(1001), ShouldBeNil)
		})
		Convey("Then periods before first are no-sample", func() {
			So(read(997), ShouldBeNil)
		})
		Convey("Then a period a full ring behind last is no-sample even though its slot holds bytes", func() {
			// Period 640 shares slot with 1000 (640 = 1000-360) — the header
			// bound must refuse to serve the newer slot's bytes as old data.
			So(read(640), ShouldBeNil)
			So(read(1000-int64(hotRing.slots)), ShouldBeNil)
		})
		Convey("Then an invalid header nils everything", func() {
			So(decodeSeriesPoint(buf, seriesHeader{}, hotRing, 998), ShouldBeNil)
		})
	})
}

func TestSeriesWindowDecoding(t *testing.T) {
	Convey("Given a hot ring with samples at the two periods before now", t, func() {
		// now period P; ring holds P-2=5 and P-1=11; P itself never flushed.
		now := time.Unix(1_753_400_000, 0)
		p := seriesPeriodIndex(now.Unix(), hotRing.period)
		buf := buildSeriesInit(hotRing, p-2, 5)
		copy(buf[8:12], encodeLastPeriod(p-1))
		binary.BigEndian.PutUint32(buf[seriesSlotOffset(seriesSlot(p-1, hotRing.slots)):], 11)

		points := decodeWindow(buf, hotRing, now, hotRing.slots)

		Convey("Then the window has one point per slot ending at now's period", func() {
			So(points, ShouldHaveLength, 360)
			So(points[len(points)-1].T, ShouldEqual, p*hotRing.period)
			So(points[0].T, ShouldEqual, (p-359)*hotRing.period)
		})
		Convey("Then the two samples land at their periods and everything else is null", func() {
			byT := map[int64]*int64{}
			nulls := 0
			for _, pt := range points {
				byT[pt.T] = pt.V
				if pt.V == nil {
					nulls++
				}
			}
			So(*byT[(p-2)*hotRing.period], ShouldEqual, 5)
			So(*byT[(p-1)*hotRing.period], ShouldEqual, 11)
			So(byT[p*hotRing.period], ShouldBeNil) // current period: accumulating, never served
			So(nulls, ShouldEqual, 358)
		})
	})
}

func TestSeriesDayMerge(t *testing.T) {
	Convey("Given rollup hours plus hot slots for the un-flushed recent hours", t, func() {
		now := time.Unix(1_753_401_800, 0) // arbitrary, mid-hour
		hourP := seriesPeriodIndex(now.Unix(), rollupRing.period)

		// Rollup: hour-23 → 100, hour-2 → 200; nothing newer flushed.
		rollup := buildSeriesInit(rollupRing, hourP-23, 100)
		copy(rollup[8:12], encodeLastPeriod(hourP-2))
		for p := hourP - 22; p <= hourP-2; p++ {
			off := seriesSlotOffset(seriesSlot(p, rollupRing.slots))
			if p == hourP-2 {
				binary.BigEndian.PutUint32(rollup[off:], 200)
			} else {
				binary.BigEndian.PutUint32(rollup[off:], seriesSentinel)
			}
		}

		// Hot: three 30s slots inside hour-1 (7, 8, 9) — sum 24, last 9.
		hotStart := (hourP - 1) * (rollupRing.period / hotRing.period)
		hot := buildSeriesInit(hotRing, hotStart, 7)
		copy(hot[8:12], encodeLastPeriod(hotStart+2))
		binary.BigEndian.PutUint32(hot[seriesSlotOffset(seriesSlot(hotStart+1, hotRing.slots)):], 8)
		binary.BigEndian.PutUint32(hot[seriesSlotOffset(seriesSlot(hotStart+2, hotRing.slots)):], 9)

		Convey("When merged as a DELTA metric (failure pulse semantics)", func() {
			points := mergeDayWindow(rollup, hot, metricDelta, now)
			So(points, ShouldHaveLength, 24)
			byT := map[int64]*int64{}
			for _, pt := range points {
				byT[pt.T] = pt.V
			}
			Convey("Then rollup hours serve their flushed values", func() {
				So(*byT[(hourP-23)*3600], ShouldEqual, 100)
				So(*byT[(hourP-2)*3600], ShouldEqual, 200)
			})
			Convey("Then the un-flushed hour is the SUM of its hot slots", func() {
				So(*byT[(hourP-1)*3600], ShouldEqual, 24)
			})
			Convey("Then hours with no data anywhere stay null", func() {
				So(byT[(hourP-10)*3600], ShouldBeNil)
				So(byT[hourP*3600], ShouldBeNil)
			})
		})

		Convey("When merged as a GAUGE metric", func() {
			points := mergeDayWindow(rollup, hot, metricGauge, now)
			byT := map[int64]*int64{}
			for _, pt := range points {
				byT[pt.T] = pt.V
			}
			Convey("Then the un-flushed hour is the LAST hot sample, not a sum", func() {
				So(*byT[(hourP-1)*3600], ShouldEqual, 9)
			})
		})
	})
}

func TestCounterDeltaRules(t *testing.T) {
	Convey("Given the §5.8 delta rules over daily counters", t, func() {
		Convey("Then a same-day increase is the difference", func() {
			So(counterDelta("2026-07-25", 100, "2026-07-25", 130), ShouldEqual, 30)
		})
		Convey("Then a same-day zero movement is zero (a real sample, distinct from no-sample)", func() {
			So(counterDelta("2026-07-25", 100, "2026-07-25", 100), ShouldEqual, 0)
		})
		Convey("Then a same-day negative diff degrades to the current value (counter deleted/reset)", func() {
			So(counterDelta("2026-07-25", 100, "2026-07-25", 8), ShouldEqual, 8)
		})
		Convey("Then a UTC-midnight rollover reports today's counter verbatim", func() {
			So(counterDelta("2026-07-24", 5000, "2026-07-25", 12), ShouldEqual, 12)
		})
	})
}

func TestSamplerDeltaLifecycle(t *testing.T) {
	Convey("Given a sampler observing two sweeps of daily counters", t, func() {
		s := newSeriesSampler(nil, 0)
		day1 := time.Date(2026, 7, 24, 23, 59, 40, 0, time.UTC)

		snaps := func(processed, failed int64) map[string]*QueueSnapshot {
			return map[string]*QueueSnapshot{
				"q1": {Queue: "q1", ProcessedToday: processed, FailedToday: failed},
			}
		}

		Convey("Then the FIRST observation yields no delta — no-sample, never zero", func() {
			d := s.computeDeltas(day1, snaps(100, 10))
			So(d, ShouldBeEmpty)

			Convey("And the second observation yields the movement", func() {
				d := s.computeDeltas(day1.Add(5*time.Second), snaps(130, 12))
				So(d["q1"].ok, ShouldBeTrue)
				So(d["q1"].processed, ShouldEqual, 30)
				So(d["q1"].failed, ShouldEqual, 2)

				Convey("And a sweep across UTC midnight uses the fresh counter verbatim", func() {
					day2 := time.Date(2026, 7, 25, 0, 0, 5, 0, time.UTC)
					d := s.computeDeltas(day2, snaps(7, 3))
					So(d["q1"].ok, ShouldBeTrue)
					So(d["q1"].processed, ShouldEqual, 7)
					So(d["q1"].failed, ShouldEqual, 3)
				})
			})
		})

		Convey("Then a deleted queue's previous counters are pruned (no ghost deltas)", func() {
			s.computeDeltas(day1, snaps(100, 10))
			s.computeDeltas(day1.Add(5*time.Second), map[string]*QueueSnapshot{})
			So(s.prev, ShouldBeEmpty)
		})
	})
}

func TestSamplerAccumulator(t *testing.T) {
	Convey("Given the in-process slot accumulator", t, func() {
		s := newSeriesSampler(nil, 0)
		var flushes []flushOp
		key := seriesKey(hotRing, "fleet", MetricPending)

		Convey("When a gauge is fed twice within one period", func() {
			s.feed(hotRing, key, metricGauge, 100, 5, &flushes)
			s.feed(hotRing, key, metricGauge, 100, 9, &flushes)
			Convey("Then nothing flushes and the accumulator holds the LAST value", func() {
				So(flushes, ShouldBeEmpty)
				So(s.acc[key].value, ShouldEqual, 9)
			})
		})

		Convey("When a delta is fed twice within one period", func() {
			key := seriesKey(hotRing, "fleet", MetricFailedDelta)
			s.feed(hotRing, key, metricDelta, 100, 5, &flushes)
			s.feed(hotRing, key, metricDelta, 100, 9, &flushes)
			Convey("Then the accumulator holds the SUM", func() {
				So(flushes, ShouldBeEmpty)
				So(s.acc[key].value, ShouldEqual, 14)
			})
		})

		Convey("When the sweep clock crosses into a later period", func() {
			s.feed(hotRing, key, metricGauge, 100, 5, &flushes)
			s.feed(hotRing, key, metricGauge, 102, 7, &flushes)
			Convey("Then the completed slot flushes with its final value", func() {
				So(flushes, ShouldHaveLength, 1)
				So(flushes[0].period, ShouldEqual, 100)
				So(flushes[0].value, ShouldEqual, uint32(5))
				So(s.acc[key].period, ShouldEqual, 102)
				So(s.acc[key].value, ShouldEqual, 7)
			})
		})

		Convey("When the clock regresses (skew)", func() {
			s.feed(hotRing, key, metricGauge, 100, 5, &flushes)
			s.feed(hotRing, key, metricGauge, 99, 3, &flushes)
			Convey("Then the regressed sample is dropped, not merged into the wrong slot", func() {
				So(flushes, ShouldBeEmpty)
				So(s.acc[key].period, ShouldEqual, 100)
				So(s.acc[key].value, ShouldEqual, 5)
			})
		})
	})
}
