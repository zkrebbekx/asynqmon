package stats

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the §5.8 time-series substrate (phase 10):
//   - the packed ring-buffer encoding (normative): per (scope, metric) one
//     Redis STRING written via SETRANGE, fixed-width uint32 slots, big-endian,
//     sentinel 0xFFFFFFFF = no-sample. Hot ring 360×30s (3h); rollup ring
//     720×1h (30d). Slot index = (unix/period) mod N.
//   - seriesSampler: the sweep-driven writer (lease holder only), including
//     the counter-delta logic (processed_delta / failed_delta from daily
//     counter differences between sweeps; first observation = no-sample,
//     never zero)
//   - ReadSeries/ReadSeriesBatch: the read path any replica serves from
//
// Layout of one series STRING (12-byte header + N fixed slots):
//
//	offset 0..1   magic "AS"
//	offset 2      version (0x01)
//	offset 3      reserved (0x00)
//	offset 4..7   FIRST period ever written (uint32 BE) — never rewritten;
//	              anchors "how much history exists" (learning badges)
//	offset 8..11  LAST period written (uint32 BE) — rewritten on every flush;
//	              the reader's validity bound
//	offset 12+i*4 slot i value (uint32 BE; 0xFFFFFFFF = no-sample)
//
// Why the header exists: slot index is (period mod N), so without a validity
// bound a ring that stopped being written would serve 3h-old (or 30d-old)
// values as if they were current. A slot for period P is only served when
// P ∈ (last−N, last] ∧ P ≥ first; everything else decodes to null. The writer
// sentinel-fills any skipped slots between consecutive flushes, so slots
// inside the valid window are always either a real sample or the sentinel.
//
// Write cadence: sweeps run every ~5s but slots are 30s/1h; the sampler
// accumulates in-process (holder-only, like the attention debounce state) and
// flushes a slot with ONE value when the sweep clock first crosses into a
// later slot. The currently-accumulating slot is therefore never in Redis —
// readers see it as null, which is honest (a partially-summed delta slot
// would misread as a dip). A lease handover mid-slot loses at most one slot
// of delta accumulation; the slot decodes as no-sample, never as a wrong
// number.
//
// Sampling tiers (§5.8, phase 12): per-queue sampling follows the sweep's
// own tiering. Tier 1 samples every queue into both rings (pre-phase-12
// behavior). Tiers 2-3 sample only the queues the tick actually refreshed:
// hot-set queues feed both rings every tick (full resolution); cold queues
// feed the ROLLUP ring only, on their rotation visit — their counter deltas
// cover the whole interval since the previous visit, so hourly rollup sums
// stay correct. Fleet and per-server series are always sampled.
// SeriesQueueCeiling remains as a hard safety ceiling (default 50,000, far
// above the old 2,000 cliff): beyond it only hot-set queues get series.
// ****************************************************************************

// Ring geometry (normative §5.8).
const (
	SeriesHotPeriodSec    = int64(30)   // hot ring: 30s slots
	SeriesHotSlots        = 360         // × 360 = 3h
	SeriesRollupPeriodSec = int64(3600) // rollup ring: 1h slots
	SeriesRollupSlots     = 720         // × 720 = 30d

	seriesHeaderLen = 12
	seriesSlotBytes = 4

	// seriesSentinel is the no-sample slot value (normative). Real values are
	// clamped to [0, 0xFFFFFFFE] so a sample can never alias the sentinel.
	seriesSentinel = uint32(0xFFFFFFFF)
	seriesMaxValue = int64(0xFFFFFFFE)
	seriesMagic0   = byte('A')
	seriesMagic1   = byte('S')
	seriesVersion  = byte(0x01)

	// defaultSeriesQueueCeiling is the §5.8 HARD safety ceiling on per-queue
	// series. Phase 12 removed the old 2,000-queue cliff: per-queue sampling
	// now follows the sweep's tiering (hot set both rings every tick, cold
	// queues rollup-only on their rotation visit), so this is a far-higher
	// backstop — above it only hot-set queues (plus fleet/server series)
	// are sampled at all.
	defaultSeriesQueueCeiling = 50000
)

// Series metric names (normative §5.8 metric set).
const (
	MetricPending        = "pending"
	MetricActive         = "active"
	MetricRetry          = "retry"
	MetricArchived       = "archived"
	MetricProcessedDelta = "processed_delta"
	MetricFailedDelta    = "failed_delta"
	MetricConsumerCount  = "consumer_count"
	MetricBusyWorkers    = "busy_workers"
	MetricConcurrency    = "concurrency"
)

// metricKind distinguishes gauges (depth/count observations — a slot holds
// the LAST observation) from deltas (counter differences — a slot holds the
// SUM of deltas observed during its period).
type metricKind int

const (
	metricGauge metricKind = iota
	metricDelta
)

// Per-scope metric catalogs. Queue and fleet scopes carry the full §5.8 set;
// server scope carries the worker-capacity pair.
var (
	queueMetrics = map[string]metricKind{
		MetricPending:        metricGauge,
		MetricActive:         metricGauge,
		MetricRetry:          metricGauge,
		MetricArchived:       metricGauge,
		MetricProcessedDelta: metricDelta,
		MetricFailedDelta:    metricDelta,
		MetricConsumerCount:  metricGauge,
	}
	serverMetrics = map[string]metricKind{
		MetricBusyWorkers: metricGauge,
		MetricConcurrency: metricGauge,
	}
	fleetMetrics = map[string]metricKind{
		MetricPending:        metricGauge,
		MetricActive:         metricGauge,
		MetricRetry:          metricGauge,
		MetricArchived:       metricGauge,
		MetricProcessedDelta: metricDelta,
		MetricFailedDelta:    metricDelta,
		MetricConsumerCount:  metricGauge, // sum of per-queue consumer counts
		MetricBusyWorkers:    metricGauge,
		MetricConcurrency:    metricGauge,
	}
)

// ringSpec describes one of the two rings.
type ringSpec struct {
	name   string // key segment: "h" (hot) / "r" (rollup)
	period int64  // slot width, seconds
	slots  int
	ttl    time.Duration // 2× the ring's window; refreshed at ttl/4 cadence
}

var (
	hotRing    = ringSpec{name: "h", period: SeriesHotPeriodSec, slots: SeriesHotSlots, ttl: 6 * time.Hour}
	rollupRing = ringSpec{name: "r", period: SeriesRollupPeriodSec, slots: SeriesRollupSlots, ttl: 60 * 24 * time.Hour}
)

func (r ringSpec) byteLen() int { return seriesHeaderLen + r.slots*seriesSlotBytes }

// ----------------------------------------------------------------------------
// Keys. asynqmon-owned (see keys.go: everything asynqmon writes lives under
// the "asynqmon:" prefix, never under "asynq:").
// ----------------------------------------------------------------------------

const (
	seriesKeyPrefix = "asynqmon:series:"

	// seriesSinceKey is a STRING holding the Unix seconds when series sampling
	// first ran against this Redis (SETNX once, never rewritten). It anchors
	// the learning badges: baseline detectors are "learning" until sampling
	// has existed long enough (DEPTH_ANOMALY 24h, CONSUMERS_DROPPED 30m), and
	// the anchor must survive restarts and lease handovers.
	seriesSinceKey = "asynqmon:series:since"
)

// Key scopes: "fleet", "q:<qname>", "s:<host:pid>".
func seriesKeyScopeFleet() string             { return "fleet" }
func seriesKeyScopeQueue(qname string) string { return "q:" + qname }
func seriesKeyScopeServer(id string) string   { return "s:" + id }

func seriesKey(ring ringSpec, keyScope, metric string) string {
	return seriesKeyPrefix + ring.name + ":" + keyScope + ":" + metric
}

// serverScopeID is the stable-ish identity of one server process for the
// series key: host:pid. (asynq's ServerInfo.ID is a fresh UUID per process
// run; host:pid survives asynq restarts of the same deployment slot, which is
// what a worker-capacity chart wants to be continuous over.)
func serverScopeID(host string, pid int) string { return fmt.Sprintf("%s:%d", host, pid) }

// ----------------------------------------------------------------------------
// Codec — pure functions (unit-tested without Redis).
// ----------------------------------------------------------------------------

// seriesPeriodIndex maps a Unix-seconds instant to its period index.
func seriesPeriodIndex(unixSec, period int64) int64 { return unixSec / period }

// seriesSlot maps a period index to its ring slot: (unix/period) mod N.
func seriesSlot(p int64, slots int) int { return int(p % int64(slots)) }

func seriesSlotOffset(slot int) int64 {
	return int64(seriesHeaderLen + slot*seriesSlotBytes)
}

// clampSeriesValue clamps a sample into the encodable range so a real value
// can never alias the sentinel (and a negative artifact never wraps).
func clampSeriesValue(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > seriesMaxValue {
		return uint32(seriesMaxValue)
	}
	return uint32(v)
}

func encodeSlotValue(v uint32) []byte {
	b := make([]byte, seriesSlotBytes)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// seriesHeader is the decoded 12-byte header. ok=false means the key is
// absent or not a series string (readers treat everything as no-sample).
type seriesHeader struct {
	ok    bool
	first int64
	last  int64
}

func decodeSeriesHeader(raw []byte) seriesHeader {
	if len(raw) < seriesHeaderLen ||
		raw[0] != seriesMagic0 || raw[1] != seriesMagic1 || raw[2] != seriesVersion {
		return seriesHeader{}
	}
	return seriesHeader{
		ok:    true,
		first: int64(binary.BigEndian.Uint32(raw[4:8])),
		last:  int64(binary.BigEndian.Uint32(raw[8:12])),
	}
}

func encodeSeriesHeader(first, last int64) []byte {
	b := make([]byte, seriesHeaderLen)
	b[0], b[1], b[2], b[3] = seriesMagic0, seriesMagic1, seriesVersion, 0
	binary.BigEndian.PutUint32(b[4:8], uint32(first))
	binary.BigEndian.PutUint32(b[8:12], uint32(last))
	return b
}

// encodeLastPeriod is the 4-byte payload of the per-flush header update
// (SETRANGE at offset 8).
func encodeLastPeriod(last int64) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(last))
	return b
}

// buildSeriesInit builds the full initial buffer for a NEW series key:
// header(first=last=p) + every slot sentinel except p's slot = value. It is
// written with a single SETRANGE at offset 0. A full-buffer init is required
// because SETRANGE zero-fills gaps in a shorter string — and a zero byte run
// would decode as value 0, a fabricated sample.
func buildSeriesInit(spec ringSpec, p int64, value uint32) []byte {
	buf := make([]byte, spec.byteLen())
	copy(buf, encodeSeriesHeader(p, p))
	for i := seriesHeaderLen; i < len(buf); i++ {
		buf[i] = 0xFF
	}
	binary.BigEndian.PutUint32(buf[seriesSlotOffset(seriesSlot(p, spec.slots)):], value)
	return buf
}

// byteRange is one contiguous SETRANGE payload of sentinel bytes.
type byteRange struct {
	offset int64
	length int
}

// seriesGapRanges returns the sentinel-fill byte ranges for the slots of
// periods (lastWritten, p) — the periods skipped between two flushes. At most
// two ranges (a wrap splits one run in two). A gap spanning the whole ring
// returns a single full-slots range.
func seriesGapRanges(spec ringSpec, lastWritten, p int64) []byteRange {
	gap := p - lastWritten - 1
	if gap <= 0 {
		return nil
	}
	if gap >= int64(spec.slots) {
		return []byteRange{{offset: seriesHeaderLen, length: spec.slots * seriesSlotBytes}}
	}
	start := seriesSlot(lastWritten+1, spec.slots)
	n := int(gap)
	if start+n <= spec.slots {
		return []byteRange{{offset: seriesSlotOffset(start), length: n * seriesSlotBytes}}
	}
	firstLen := spec.slots - start
	return []byteRange{
		{offset: seriesSlotOffset(start), length: firstLen * seriesSlotBytes},
		{offset: seriesSlotOffset(0), length: (n - firstLen) * seriesSlotBytes},
	}
}

func sentinelBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// decodeSeriesPoint extracts period p's value from a raw ring string.
// Returns nil (no-sample) when the header is invalid, p is outside the valid
// window (p > last, p ≤ last−N, p < first), or the slot holds the sentinel.
func decodeSeriesPoint(raw []byte, h seriesHeader, spec ringSpec, p int64) *int64 {
	if !h.ok || p > h.last || p < h.first || p <= h.last-int64(spec.slots) {
		return nil
	}
	off := seriesSlotOffset(seriesSlot(p, spec.slots))
	if int64(len(raw)) < off+seriesSlotBytes {
		return nil
	}
	v := binary.BigEndian.Uint32(raw[off : off+seriesSlotBytes])
	if v == seriesSentinel {
		return nil
	}
	out := int64(v)
	return &out
}

// ----------------------------------------------------------------------------
// Counter deltas — pure logic (unit-tested without Redis).
// ----------------------------------------------------------------------------

// counterPrev is the previous sweep's daily-counter observation for one queue.
type counterPrev struct {
	date      string // UTC date the counters belong to
	processed int64
	failed    int64
}

// queueDeltas is the per-sweep counter movement for one queue. ok=false on
// the first observation: with no previous counter there IS no delta — the
// slot stays no-sample, never zero (§5.8).
type queueDeltas struct {
	processed int64
	failed    int64
	ok        bool
}

// counterDelta computes one counter's movement given the previous
// observation. Rules:
//   - same UTC date: delta = cur − prev; a negative diff (counter deleted /
//     restarted) degrades to cur — the counter counts from zero again.
//   - date rolled over: delta = cur (today's counter starts at zero at UTC
//     midnight, so cur IS the movement since the roll).
func counterDelta(prevDate string, prev int64, today string, cur int64) int64 {
	if prevDate == today {
		d := cur - prev
		if d < 0 {
			return cur
		}
		return d
	}
	return cur
}

// ----------------------------------------------------------------------------
// The sampler (sweep-driven writer; holder-only state).
// ----------------------------------------------------------------------------

// slotAcc is one series' in-process accumulator: the value being built for
// the period the sweep clock is currently inside. queue is the owning queue
// name for queue-scoped series ("" for fleet/server scopes) — the retention
// rule needs it: a cold queue's accumulator must survive ticks that do not
// visit the queue (phase-12 rotation), while a DELETED queue's accumulator
// still flushes its partial slot and is forgotten.
type slotAcc struct {
	spec   ringSpec
	period int64
	value  int64
	kind   metricKind
	queue  string
}

// merge folds a new sample of the same period into the accumulator.
func (a *slotAcc) merge(v int64) {
	if a.kind == metricDelta {
		a.value += v
	} else {
		a.value = v
	}
}

// seriesSample is one (scope, metric, value) observation of a sweep. queue
// is set for queue-scoped samples (retention rule); rollupOnly marks cold
// queues' samples (phase-12 tiering: rotation visits feed the rollup ring
// only — writing 30s hot slots for a queue visited every few minutes would
// produce a misleading mostly-null hot ring at full write cost).
type seriesSample struct {
	keyScope   string
	metric     string
	kind       metricKind
	value      int64
	queue      string
	rollupOnly bool
}

// flushOp is one completed slot ready to be written.
type flushOp struct {
	key    string
	spec   ringSpec
	period int64
	value  uint32
}

type seriesSampler struct {
	rc           redis.UniversalClient
	queueCeiling int

	// flushCap bounds how many flush OPERATIONS (each ~2 SETRANGE commands)
	// one sample() call may issue in tiers 2-3 (derived from the §5.1
	// auxiliary budget reserve by NewEngine; 0 = unlimited). Every hot-ring
	// key crosses its 30s slot boundary on the same tick, so without the cap
	// the flush load arrives as a single burst of ~2×keys commands — scale
	// validation (docs/SCALE.md) measured boundary ticks at ~1.6× the
	// command budget. Excess flushes queue in pending and drain over the
	// following ticks. Tier 1 is uncapped (the budget does not govern
	// tier 1).
	flushCap int
	pending  []flushOp // FIFO of completed slots awaiting their write turn

	acc      map[string]*slotAcc     // redis key → accumulator
	headers  map[string]seriesHeader // redis key → last known persisted header
	ttlAt    map[string]time.Time    // redis key → when TTL was last set
	prev     map[string]counterPrev  // qname → previous daily counters
	sinceSet bool                    // asynqmon:series:since SETNX done
}

func newSeriesSampler(rc redis.UniversalClient, queueCeiling, flushCap int) *seriesSampler {
	if queueCeiling <= 0 {
		queueCeiling = defaultSeriesQueueCeiling
	}
	return &seriesSampler{
		rc:           rc,
		queueCeiling: queueCeiling,
		flushCap:     flushCap,
		acc:          make(map[string]*slotAcc),
		headers:      make(map[string]seriesHeader),
		ttlAt:        make(map[string]time.Time),
		prev:         make(map[string]counterPrev),
	}
}

// computeDeltas turns this sweep's daily counters into per-queue deltas and
// updates the previous-counter memory. snaps holds only the queues this tick
// REFRESHED (phase 12) — a cold queue's delta therefore covers the whole
// span since its previous rotation visit, which is exactly what its rollup
// slot should accumulate. universe is the full current fleet: only queues
// gone from the fleet are pruned from the counter memory (pruning by snaps
// would wipe every cold queue's memory every tick).
func (s *seriesSampler) computeDeltas(now time.Time, snaps map[string]*QueueSnapshot, universe map[string]bool) map[string]queueDeltas {
	today := now.UTC().Format("2006-01-02")
	out := make(map[string]queueDeltas, len(snaps))
	for q, snap := range snaps {
		if p, seen := s.prev[q]; seen {
			out[q] = queueDeltas{
				processed: counterDelta(p.date, p.processed, today, snap.ProcessedToday),
				failed:    counterDelta(p.date, p.failed, today, snap.FailedToday),
				ok:        true,
			}
		}
		s.prev[q] = counterPrev{date: today, processed: snap.ProcessedToday, failed: snap.FailedToday}
	}
	for q := range s.prev {
		if !universe[q] {
			delete(s.prev, q)
		}
	}
	return out
}

// seriesTick tells the sampler how the sweep tiered this tick (§5.1/§5.8):
// which queues are hot (full resolution), the fleet size (hard-ceiling
// check), and the tier. Tier 1 samples every refreshed queue into both
// rings; snaps itself already contains only refreshed queues.
type seriesTick struct {
	tier      int
	fleetSize int
	hot       map[string]bool
}

// buildSamples assembles the sweep's full sample set: fleet always, servers
// always, refreshed queues per the tick's tiering (tier 1: both rings; tiers
// 2-3: hot both rings, cold rollup-only; above the hard ceiling: hot only).
func (s *seriesSampler) buildSamples(snaps map[string]*QueueSnapshot, deltas map[string]queueDeltas, fleet *FleetSnapshot, servers []*asynq.ServerInfo, tick seriesTick) []seriesSample {
	samples := make([]seriesSample, 0, 16+len(snaps)*7+len(servers)*2)

	// Fleet aggregates. Delta metrics are the SUM of per-queue deltas (only
	// queues that have one — robust to queue deletion mid-day, unlike
	// re-diffing the summed fleet counters). consumer_count is the sum of
	// per-queue consumer counts.
	fleetScope := seriesKeyScopeFleet()
	fleetConsumers := int64(0)
	for _, snap := range snaps {
		fleetConsumers += int64(snap.Consumers)
	}
	var dp, df int64
	haveDelta := false
	for _, d := range deltas {
		if d.ok {
			dp += d.processed
			df += d.failed
			haveDelta = true
		}
	}
	samples = append(samples,
		seriesSample{keyScope: fleetScope, metric: MetricPending, kind: metricGauge, value: fleet.Pending},
		seriesSample{keyScope: fleetScope, metric: MetricActive, kind: metricGauge, value: fleet.Active},
		seriesSample{keyScope: fleetScope, metric: MetricRetry, kind: metricGauge, value: fleet.Retry},
		seriesSample{keyScope: fleetScope, metric: MetricArchived, kind: metricGauge, value: fleet.Archived},
		seriesSample{keyScope: fleetScope, metric: MetricConsumerCount, kind: metricGauge, value: fleetConsumers},
		seriesSample{keyScope: fleetScope, metric: MetricBusyWorkers, kind: metricGauge, value: int64(fleet.WorkersBusy)},
		seriesSample{keyScope: fleetScope, metric: MetricConcurrency, kind: metricGauge, value: int64(fleet.WorkersTotal)},
	)
	if haveDelta {
		samples = append(samples,
			seriesSample{keyScope: fleetScope, metric: MetricProcessedDelta, kind: metricDelta, value: dp},
			seriesSample{keyScope: fleetScope, metric: MetricFailedDelta, kind: metricDelta, value: df},
		)
	}

	for _, srv := range servers {
		scope := seriesKeyScopeServer(serverScopeID(srv.Host, srv.PID))
		samples = append(samples,
			seriesSample{keyScope: scope, metric: MetricBusyWorkers, kind: metricGauge, value: int64(len(srv.ActiveWorkers))},
			seriesSample{keyScope: scope, metric: MetricConcurrency, kind: metricGauge, value: int64(srv.Concurrency)},
		)
	}

	// Per-queue samples over the tick's REFRESHED queues (§5.8 phase-12
	// tiering — the cliff is gone): tier 1 feeds both rings; tiers 2-3 feed
	// hot queues both rings and cold queues rollup-only; above the hard
	// ceiling only hot queues are sampled at all.
	for q, snap := range snaps {
		hot := tick.tier <= 1 || tick.hot[q]
		if !hot && tick.fleetSize > s.queueCeiling {
			continue // hard safety ceiling (§5.8)
		}
		rollupOnly := tick.tier > 1 && !hot
		scope := seriesKeyScopeQueue(q)
		samples = append(samples,
			seriesSample{keyScope: scope, metric: MetricPending, kind: metricGauge, value: snap.Pending, queue: q, rollupOnly: rollupOnly},
			seriesSample{keyScope: scope, metric: MetricActive, kind: metricGauge, value: snap.Active, queue: q, rollupOnly: rollupOnly},
			seriesSample{keyScope: scope, metric: MetricRetry, kind: metricGauge, value: snap.Retry, queue: q, rollupOnly: rollupOnly},
			seriesSample{keyScope: scope, metric: MetricArchived, kind: metricGauge, value: snap.Archived, queue: q, rollupOnly: rollupOnly},
			seriesSample{keyScope: scope, metric: MetricConsumerCount, kind: metricGauge, value: int64(snap.Consumers), queue: q, rollupOnly: rollupOnly},
		)
		if d := deltas[q]; d.ok {
			samples = append(samples,
				seriesSample{keyScope: scope, metric: MetricProcessedDelta, kind: metricDelta, value: d.processed, queue: q, rollupOnly: rollupOnly},
				seriesSample{keyScope: scope, metric: MetricFailedDelta, kind: metricDelta, value: d.failed, queue: q, rollupOnly: rollupOnly},
			)
		}
	}
	return samples
}

// feed advances one series' accumulator with a sample at period p, appending
// a flushOp when the sweep clock crossed into a later period. Regressions
// (clock skew) are dropped rather than corrupting an already-flushed slot.
func (s *seriesSampler) feed(spec ringSpec, key, queue string, kind metricKind, p, v int64, flushes *[]flushOp) {
	a := s.acc[key]
	if a == nil {
		s.acc[key] = &slotAcc{spec: spec, period: p, value: v, kind: kind, queue: queue}
		return
	}
	switch {
	case p == a.period:
		a.merge(v)
	case p > a.period:
		*flushes = append(*flushes, flushOp{key: key, spec: a.spec, period: a.period, value: clampSeriesValue(a.value)})
		s.acc[key] = &slotAcc{spec: spec, period: p, value: v, kind: kind, queue: queue}
	}
}

// sample runs once per sweep on the lease holder. Returns the number of
// Redis read and write commands issued (folded into SweepStats).
//
// Per-flush cost: 2 SETRANGE (header last + slot) for known keys, 1 SETRANGE
// for new keys (full init buffer), + PEXPIRE at ttl/4 cadence, + sentinel
// SETRANGEs only when slots were skipped. Since phase 12 the flush load
// follows the sweep's shard rotation: only refreshed queues feed series, hot
// queues at both resolutions, cold queues rollup-only (one flush per rotation
// visit at most) — the old pre-governor note about tier-1-at-the-ceiling
// exceeding the budget is resolved by the rotation itself.
func (s *seriesSampler) sample(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, universe map[string]bool, tick seriesTick, fleet *FleetSnapshot, servers []*asynq.ServerInfo) (reads, writes int, err error) {
	// Anchor the learning clock exactly once (survives restarts/handovers).
	if !s.sinceSet {
		if err := s.rc.SetNX(ctx, seriesSinceKey, now.Unix(), 0).Err(); err != nil {
			return 0, 1, fmt.Errorf("writing series since anchor: %w", err)
		}
		writes++
		s.sinceSet = true
	}

	deltas := s.computeDeltas(now, snaps, universe)
	samples := s.buildSamples(snaps, deltas, fleet, servers, tick)

	var flushes []flushOp
	seen := make(map[string]struct{}, len(samples)*2)
	unix := now.Unix()
	for _, ring := range []ringSpec{hotRing, rollupRing} {
		if ring.name == hotRing.name {
			// Hot ring: full-resolution samples only.
			p := seriesPeriodIndex(unix, ring.period)
			for _, smp := range samples {
				if smp.rollupOnly {
					continue
				}
				key := seriesKey(ring, smp.keyScope, smp.metric)
				seen[key] = struct{}{}
				s.feed(ring, key, smp.queue, smp.kind, p, smp.value, &flushes)
			}
			continue
		}
		p := seriesPeriodIndex(unix, ring.period)
		for _, smp := range samples {
			key := seriesKey(ring, smp.keyScope, smp.metric)
			seen[key] = struct{}{}
			s.feed(ring, key, smp.queue, smp.kind, p, smp.value, &flushes)
		}
	}

	// Accumulators not fed this tick: a queue still in the fleet is merely
	// cold (awaiting its rotation visit) — its accumulator MUST survive, or
	// the partial slot would flush early and the regression guard would drop
	// the rest of the hour's data. Everything else (deleted queue, dead
	// server) flushes its partial slot once and is forgotten.
	for key, a := range s.acc {
		if _, ok := seen[key]; ok {
			continue
		}
		if a.queue != "" && universe[a.queue] {
			continue // cold queue between rotation visits: keep accumulating
		}
		flushes = append(flushes, flushOp{key: key, spec: a.spec, period: a.period, value: clampSeriesValue(a.value)})
		delete(s.acc, key)
	}
	sort.Slice(flushes, func(i, j int) bool { return flushes[i].key < flushes[j].key })
	s.pending = append(s.pending, flushes...)
	if len(s.pending) == 0 {
		return reads, writes, nil
	}

	// Drain the flush queue. Tiers 2-3 drain at a FLAT flushCap per tick
	// (auxiliary budget reserve). The cap must be flat — not scaled to the
	// backlog — because of the hourly cold-rollup hump measured by scale
	// validation (docs/SCALE.md): at every UTC hour boundary every cold
	// queue's rollup accumulator period advances, so each queue's next
	// rotation visit flushes its previous-hour slots. At 5,000 queues that
	// is a sustained ~250 flush-ops/tick inflow for one full rotation every
	// hour; a backlog-proportional drain simply matched that inflow and
	// spent ~1.4× the tick budget. Under the flat cap the backlog absorbs
	// the hump (peak ≈ 7 × fleet ops) and clears long before the next hour
	// (drain capacity per hour far exceeds total hourly inflow at any
	// interval ≤ ~10s). A delayed flush only means a completed slot appears
	// in Redis late; readers already treat unflushed slots as no-sample.
	// Order is FIFO, so per-key period order is preserved and the
	// regression guard below stays correct.
	//
	// Memory-relief valve: at pathological intervals (≳15s, where every
	// tick crosses a hot-slot boundary) inflow can exceed the flat cap
	// forever. Past a generous backlog ceiling the excess drains
	// immediately — at those intervals the per-tick budget is huge, so the
	// extra spend still fits the budget envelope.
	drain := len(s.pending)
	if tick.tier > 1 && s.flushCap > 0 && drain > s.flushCap {
		drain = s.flushCap
		valve := 8 * s.flushCap
		if v := 8 * tick.fleetSize; v > valve {
			valve = v
		}
		if excess := len(s.pending) - valve; excess > drain {
			drain = excess
		}
	}
	batch := s.pending[:drain]
	s.pending = append(make([]flushOp, 0, len(s.pending)-drain), s.pending[drain:]...)

	// Probe persisted headers for keys we have not seen this holder session:
	// the init-vs-update decision and the FIRST period must reflect what a
	// previous holder already wrote.
	var probes []string
	for _, f := range batch {
		if _, known := s.headers[f.key]; !known {
			probes = append(probes, f.key)
		}
	}
	if len(probes) > 0 {
		pipe := s.rc.Pipeline()
		cmds := make([]*redis.StringCmd, len(probes))
		for i, key := range probes {
			cmds[i] = pipe.GetRange(ctx, key, 0, seriesHeaderLen-1)
			reads++
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return reads, writes, fmt.Errorf("probing series headers: %w", err)
		}
		for i, key := range probes {
			raw, _ := cmds[i].Bytes()
			s.headers[key] = decodeSeriesHeader(raw)
		}
	}

	pipe := s.rc.Pipeline()
	for _, f := range batch {
		h := s.headers[f.key]
		switch {
		case !h.ok:
			// New key: one full-buffer SETRANGE (header + sentinels + value).
			pipe.SetRange(ctx, f.key, 0, string(buildSeriesInit(f.spec, f.period, f.value)))
			writes++
			s.headers[f.key] = seriesHeader{ok: true, first: f.period, last: f.period}
			pipe.PExpire(ctx, f.key, f.spec.ttl)
			writes++
			s.ttlAt[f.key] = now
		case f.period <= h.last:
			// Regression vs the persisted ring (another holder raced ahead,
			// or clock skew): drop rather than rewrite history.
			continue
		default:
			for _, gr := range seriesGapRanges(f.spec, h.last, f.period) {
				pipe.SetRange(ctx, f.key, gr.offset, string(sentinelBytes(gr.length)))
				writes++
			}
			pipe.SetRange(ctx, f.key, 8, string(encodeLastPeriod(f.period)))
			pipe.SetRange(ctx, f.key, seriesSlotOffset(seriesSlot(f.period, f.spec.slots)), string(encodeSlotValue(f.value)))
			writes += 2
			s.headers[f.key] = seriesHeader{ok: true, first: h.first, last: f.period}
			if now.Sub(s.ttlAt[f.key]) > f.spec.ttl/4 {
				pipe.PExpire(ctx, f.key, f.spec.ttl)
				writes++
				s.ttlAt[f.key] = now
			}
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Forget assumed headers so the next sweep re-probes instead of
		// trusting writes that may not have landed.
		for _, f := range batch {
			delete(s.headers, f.key)
		}
		return reads, writes, fmt.Errorf("writing series flushes: %w", err)
	}
	return reads, writes, nil
}

// ----------------------------------------------------------------------------
// Read path (any replica).
// ----------------------------------------------------------------------------

// SeriesScope is a parsed API scope.
type SeriesScope struct {
	Kind string // "fleet" | "queue" | "server"
	Name string // queue name / server id; empty for fleet
}

// ParseSeriesScope parses the API scope grammar: fleet | queue:<name> |
// server:<id>.
func ParseSeriesScope(s string) (SeriesScope, error) {
	if s == "fleet" {
		return SeriesScope{Kind: "fleet"}, nil
	}
	if name, ok := strings.CutPrefix(s, "queue:"); ok && name != "" {
		return SeriesScope{Kind: "queue", Name: name}, nil
	}
	if id, ok := strings.CutPrefix(s, "server:"); ok && id != "" {
		return SeriesScope{Kind: "server", Name: id}, nil
	}
	return SeriesScope{}, fmt.Errorf("invalid series scope %q (want fleet, queue:<name> or server:<id>)", s)
}

func (sc SeriesScope) String() string {
	switch sc.Kind {
	case "queue":
		return "queue:" + sc.Name
	case "server":
		return "server:" + sc.Name
	default:
		return "fleet"
	}
}

func (sc SeriesScope) keyScope() string {
	switch sc.Kind {
	case "queue":
		return seriesKeyScopeQueue(sc.Name)
	case "server":
		return seriesKeyScopeServer(sc.Name)
	default:
		return seriesKeyScopeFleet()
	}
}

// SeriesMetricKind validates a metric against a scope and returns its kind.
func SeriesMetricKind(sc SeriesScope, metric string) (metricKind, bool) {
	var m map[string]metricKind
	switch sc.Kind {
	case "queue":
		m = queueMetrics
	case "server":
		m = serverMetrics
	default:
		m = fleetMetrics
	}
	k, ok := m[metric]
	return k, ok
}

// Series windows.
const (
	SeriesWindow3h  = "3h"  // hot ring verbatim: 360 × 30s
	SeriesWindow24h = "24h" // merged: 24 × 1h (rollup, hot-filled for recent hours)
	SeriesWindow30d = "30d" // rollup ring verbatim: 720 × 1h
)

// SeriesPoint is one period's sample. V nil = no-sample (never zero).
type SeriesPoint struct {
	T int64  // period start, Unix seconds
	V *int64 // nil = no-sample
}

// Series is one read result.
type Series struct {
	Scope  SeriesScope
	Metric string
	Window string
	Period int64 // seconds per point
	Points []SeriesPoint
	// LearningUntil is nonzero while series sampling has existed for less
	// than 24h against this Redis (the DEPTH_ANOMALY learning horizon) —
	// baselines derived from this series are not yet trustworthy.
	LearningUntil time.Time
}

// SeriesSpec is one batch-read request.
type SeriesSpec struct {
	Scope     SeriesScope
	Metric    string
	Window    string
	MaxPoints int // 0 = full window; else the last N points
}

// ValidateSeriesSpec checks scope/metric/window/points coherence.
func ValidateSeriesSpec(spec SeriesSpec) error {
	if _, ok := SeriesMetricKind(spec.Scope, spec.Metric); !ok {
		return fmt.Errorf("metric %q is not collected for scope %q", spec.Metric, spec.Scope.String())
	}
	switch spec.Window {
	case SeriesWindow3h, SeriesWindow24h, SeriesWindow30d:
	default:
		return fmt.Errorf("invalid window %q (want 3h, 24h or 30d)", spec.Window)
	}
	if spec.MaxPoints < 0 || spec.MaxPoints > SeriesRollupSlots {
		return fmt.Errorf("points must be between 1 and %d", SeriesRollupSlots)
	}
	return nil
}

// ReadSeries reads one series. See ReadSeriesBatch.
func ReadSeries(ctx context.Context, rc redis.UniversalClient, spec SeriesSpec, now time.Time) (*Series, error) {
	out, err := ReadSeriesBatch(ctx, rc, []SeriesSpec{spec}, now)
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// ReadSeriesBatch reads many series in one pipelined pass (sparkline pages).
// A series that was never written decodes to all-null points — absent data is
// no-sample, not zero, not an error.
func ReadSeriesBatch(ctx context.Context, rc redis.UniversalClient, specs []SeriesSpec, now time.Time) ([]*Series, error) {
	for _, spec := range specs {
		if err := ValidateSeriesSpec(spec); err != nil {
			return nil, err
		}
	}

	// One pipeline: per spec the ring GET(s) — 24h needs rollup + hot — plus
	// the since anchor for learning stamps.
	pipe := rc.Pipeline()
	sinceCmd := pipe.Get(ctx, seriesSinceKey)
	type specCmds struct {
		primary *redis.StringCmd // hot (3h) or rollup (24h, 30d)
		hot     *redis.StringCmd // 24h only
	}
	cmds := make([]specCmds, len(specs))
	for i, spec := range specs {
		ks := spec.Scope.keyScope()
		switch spec.Window {
		case SeriesWindow3h:
			cmds[i].primary = pipe.Get(ctx, seriesKey(hotRing, ks, spec.Metric))
		case SeriesWindow30d:
			cmds[i].primary = pipe.Get(ctx, seriesKey(rollupRing, ks, spec.Metric))
		case SeriesWindow24h:
			cmds[i].primary = pipe.Get(ctx, seriesKey(rollupRing, ks, spec.Metric))
			cmds[i].hot = pipe.Get(ctx, seriesKey(hotRing, ks, spec.Metric))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}

	// Learning stamp: sampling must have existed ≥24h for ring-derived
	// baselines to be meaningful. Absent anchor = sampling never ran here.
	var learningUntil time.Time
	if v, err := sinceCmd.Int64(); err == nil && v > 0 {
		if until := time.Unix(v, 0).Add(24 * time.Hour); now.Before(until) {
			learningUntil = until
		}
	} else {
		learningUntil = now.Add(24 * time.Hour)
	}

	out := make([]*Series, len(specs))
	for i, spec := range specs {
		kind, _ := SeriesMetricKind(spec.Scope, spec.Metric)
		raw, _ := cmds[i].primary.Bytes()
		var points []SeriesPoint
		var period int64
		switch spec.Window {
		case SeriesWindow3h:
			period = hotRing.period
			points = decodeWindow(raw, hotRing, now, hotRing.slots)
		case SeriesWindow30d:
			period = rollupRing.period
			points = decodeWindow(raw, rollupRing, now, rollupRing.slots)
		case SeriesWindow24h:
			period = rollupRing.period
			hotRaw, _ := cmds[i].hot.Bytes()
			points = mergeDayWindow(raw, hotRaw, kind, now)
		}
		if spec.MaxPoints > 0 && len(points) > spec.MaxPoints {
			points = points[len(points)-spec.MaxPoints:]
		}
		out[i] = &Series{
			Scope:         spec.Scope,
			Metric:        spec.Metric,
			Window:        spec.Window,
			Period:        period,
			Points:        points,
			LearningUntil: learningUntil,
		}
	}
	return out, nil
}

// decodeWindow decodes the last `count` periods ending at now's period.
func decodeWindow(raw []byte, spec ringSpec, now time.Time, count int) []SeriesPoint {
	h := decodeSeriesHeader(raw)
	nowP := seriesPeriodIndex(now.Unix(), spec.period)
	points := make([]SeriesPoint, 0, count)
	for p := nowP - int64(count) + 1; p <= nowP; p++ {
		points = append(points, SeriesPoint{T: p * spec.period, V: decodeSeriesPoint(raw, h, spec, p)})
	}
	return points
}

// mergeDayWindow builds the 24 × 1h merged view (§3.1 Region D): rollup
// hourly slots where present; hours the rollup has not flushed yet (the
// recent ~1-3h and the current hour) are filled from hot slots — delta
// metrics sum the hour's non-null 30s slots, gauges take the last non-null.
func mergeDayWindow(rollupRaw, hotRaw []byte, kind metricKind, now time.Time) []SeriesPoint {
	rh := decodeSeriesHeader(rollupRaw)
	hh := decodeSeriesHeader(hotRaw)
	nowH := seriesPeriodIndex(now.Unix(), rollupRing.period)
	points := make([]SeriesPoint, 0, 24)
	for hp := nowH - 23; hp <= nowH; hp++ {
		v := decodeSeriesPoint(rollupRaw, rh, rollupRing, hp)
		if v == nil {
			v = aggregateHotHour(hotRaw, hh, kind, hp)
		}
		points = append(points, SeriesPoint{T: hp * rollupRing.period, V: v})
	}
	return points
}

// aggregateHotHour folds the hot slots inside one rollup hour. All-null
// (or hour outside the hot window) → nil.
func aggregateHotHour(hotRaw []byte, hh seriesHeader, kind metricKind, hourP int64) *int64 {
	perHour := rollupRing.period / hotRing.period // 120
	startP := hourP * perHour
	var sum int64
	var last *int64
	found := false
	for p := startP; p < startP+perHour; p++ {
		if v := decodeSeriesPoint(hotRaw, hh, hotRing, p); v != nil {
			sum += *v
			last = v
			found = true
		}
	}
	if !found {
		return nil
	}
	if kind == metricDelta {
		return &sum
	}
	return last
}
