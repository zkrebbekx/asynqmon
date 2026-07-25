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
// Tier guard (§5.8 sampling tiers): per-queue series are sampled only while
// the fleet is within SeriesQueueCeiling (default 2,000 — the §5.1 tier-1
// bound). Fleet and per-server series are always sampled. Hot-set tiering for
// larger fleets (sample the attention-flagged/top-K hot set at full
// resolution, cold queues rollup-only) is phase 12 — see the phase-12 notes
// in the sampler below.
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
	seriesSentinel  = uint32(0xFFFFFFFF)
	seriesMaxValue  = int64(0xFFFFFFFE)
	seriesMagic0    = byte('A')
	seriesMagic1    = byte('S')
	seriesVersion   = byte(0x01)

	// defaultSeriesQueueCeiling is the per-queue-series tier guard (§5.8).
	defaultSeriesQueueCeiling = 2000
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
// the period the sweep clock is currently inside.
type slotAcc struct {
	spec   ringSpec
	period int64
	value  int64
	kind   metricKind
}

// merge folds a new sample of the same period into the accumulator.
func (a *slotAcc) merge(v int64) {
	if a.kind == metricDelta {
		a.value += v
	} else {
		a.value = v
	}
}

// seriesSample is one (scope, metric, value) observation of a sweep.
type seriesSample struct {
	keyScope string
	metric   string
	kind     metricKind
	value    int64
}

// flushOp is one completed slot ready to be written.
type flushOp struct {
	key   string
	spec  ringSpec
	period int64
	value uint32
}

type seriesSampler struct {
	rc           redis.UniversalClient
	queueCeiling int

	acc     map[string]*slotAcc     // redis key → accumulator
	headers map[string]seriesHeader // redis key → last known persisted header
	ttlAt   map[string]time.Time    // redis key → when TTL was last set
	prev    map[string]counterPrev  // qname → previous daily counters
	sinceSet bool                   // asynqmon:series:since SETNX done
}

func newSeriesSampler(rc redis.UniversalClient, queueCeiling int) *seriesSampler {
	if queueCeiling <= 0 {
		queueCeiling = defaultSeriesQueueCeiling
	}
	return &seriesSampler{
		rc:           rc,
		queueCeiling: queueCeiling,
		acc:          make(map[string]*slotAcc),
		headers:      make(map[string]seriesHeader),
		ttlAt:        make(map[string]time.Time),
		prev:         make(map[string]counterPrev),
	}
}

// computeDeltas turns this sweep's daily counters into per-queue deltas and
// updates the previous-counter memory. Queues gone from the fleet are pruned.
func (s *seriesSampler) computeDeltas(now time.Time, snaps map[string]*QueueSnapshot) map[string]queueDeltas {
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
		if _, ok := snaps[q]; !ok {
			delete(s.prev, q)
		}
	}
	return out
}

// buildSamples assembles the sweep's full sample set: fleet always, servers
// always, queues only under the tier guard.
func (s *seriesSampler) buildSamples(snaps map[string]*QueueSnapshot, deltas map[string]queueDeltas, fleet *FleetSnapshot, servers []*asynq.ServerInfo) []seriesSample {
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
		seriesSample{fleetScope, MetricPending, metricGauge, fleet.Pending},
		seriesSample{fleetScope, MetricActive, metricGauge, fleet.Active},
		seriesSample{fleetScope, MetricRetry, metricGauge, fleet.Retry},
		seriesSample{fleetScope, MetricArchived, metricGauge, fleet.Archived},
		seriesSample{fleetScope, MetricConsumerCount, metricGauge, fleetConsumers},
		seriesSample{fleetScope, MetricBusyWorkers, metricGauge, int64(fleet.WorkersBusy)},
		seriesSample{fleetScope, MetricConcurrency, metricGauge, int64(fleet.WorkersTotal)},
	)
	if haveDelta {
		samples = append(samples,
			seriesSample{fleetScope, MetricProcessedDelta, metricDelta, dp},
			seriesSample{fleetScope, MetricFailedDelta, metricDelta, df},
		)
	}

	for _, srv := range servers {
		scope := seriesKeyScopeServer(serverScopeID(srv.Host, srv.PID))
		samples = append(samples,
			seriesSample{scope, MetricBusyWorkers, metricGauge, int64(len(srv.ActiveWorkers))},
			seriesSample{scope, MetricConcurrency, metricGauge, int64(srv.Concurrency)},
		)
	}

	// Tier guard (§5.8): per-queue series only within the ceiling. Above it,
	// fleet + server series keep working; per-queue sparklines read as
	// no-sample. Phase 12 replaces this cliff with hot-set tiering.
	if len(snaps) <= s.queueCeiling {
		for q, snap := range snaps {
			scope := seriesKeyScopeQueue(q)
			samples = append(samples,
				seriesSample{scope, MetricPending, metricGauge, snap.Pending},
				seriesSample{scope, MetricActive, metricGauge, snap.Active},
				seriesSample{scope, MetricRetry, metricGauge, snap.Retry},
				seriesSample{scope, MetricArchived, metricGauge, snap.Archived},
				seriesSample{scope, MetricConsumerCount, metricGauge, int64(snap.Consumers)},
			)
			if d := deltas[q]; d.ok {
				samples = append(samples,
					seriesSample{scope, MetricProcessedDelta, metricDelta, d.processed},
					seriesSample{scope, MetricFailedDelta, metricDelta, d.failed},
				)
			}
		}
	}
	return samples
}

// feed advances one series' accumulator with a sample at period p, appending
// a flushOp when the sweep clock crossed into a later period. Regressions
// (clock skew) are dropped rather than corrupting an already-flushed slot.
func (s *seriesSampler) feed(spec ringSpec, key string, kind metricKind, p, v int64, flushes *[]flushOp) {
	a := s.acc[key]
	if a == nil {
		s.acc[key] = &slotAcc{spec: spec, period: p, value: v, kind: kind}
		return
	}
	switch {
	case p == a.period:
		a.merge(v)
	case p > a.period:
		*flushes = append(*flushes, flushOp{key: key, spec: a.spec, period: a.period, value: clampSeriesValue(a.value)})
		s.acc[key] = &slotAcc{spec: spec, period: p, value: v, kind: kind}
	}
}

// sample runs once per sweep on the lease holder. Returns the number of
// Redis read and write commands issued (folded into SweepStats).
//
// Per-flush cost: 2 SETRANGE (header last + slot) for known keys, 1 SETRANGE
// for new keys (full init buffer), + PEXPIRE at ttl/4 cadence, + sentinel
// SETRANGEs only when slots were skipped. Hot flushes happen once per 30s per
// series; at the 2k-queue tier-guard ceiling that is ~2×7×2000/30 ≈ 930
// cmd/s of writes on top of the sweep's reads — phase 12's command-budget
// governor should fold series flushes into its shard rotation (documented
// deviation: tier-1 + full per-queue series slightly exceeds the default
// 2,000 cmd/s budget at exactly the ceiling).
func (s *seriesSampler) sample(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, fleet *FleetSnapshot, servers []*asynq.ServerInfo) (reads, writes int, err error) {
	// Anchor the learning clock exactly once (survives restarts/handovers).
	if !s.sinceSet {
		if err := s.rc.SetNX(ctx, seriesSinceKey, now.Unix(), 0).Err(); err != nil {
			return 0, 1, fmt.Errorf("writing series since anchor: %w", err)
		}
		writes++
		s.sinceSet = true
	}

	deltas := s.computeDeltas(now, snaps)
	samples := s.buildSamples(snaps, deltas, fleet, servers)

	var flushes []flushOp
	seen := make(map[string]struct{}, len(samples)*2)
	unix := now.Unix()
	for _, ring := range []ringSpec{hotRing, rollupRing} {
		p := seriesPeriodIndex(unix, ring.period)
		for _, smp := range samples {
			key := seriesKey(ring, smp.keyScope, smp.metric)
			seen[key] = struct{}{}
			s.feed(ring, key, smp.kind, p, smp.value, &flushes)
		}
	}

	// Series that stopped being sampled (deleted queue, dead server, tier
	// guard engaged): flush their partial slot once, then forget them.
	for key, a := range s.acc {
		if _, ok := seen[key]; !ok {
			flushes = append(flushes, flushOp{key: key, spec: a.spec, period: a.period, value: clampSeriesValue(a.value)})
			delete(s.acc, key)
		}
	}
	if len(flushes) == 0 {
		return reads, writes, nil
	}
	sort.Slice(flushes, func(i, j int) bool { return flushes[i].key < flushes[j].key })

	// Probe persisted headers for keys we have not seen this holder session:
	// the init-vs-update decision and the FIRST period must reflect what a
	// previous holder already wrote.
	var probes []string
	for _, f := range flushes {
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
	for _, f := range flushes {
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
		for _, f := range flushes {
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
