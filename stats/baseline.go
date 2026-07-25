package stats

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the baseline substrate for the phase-10 detectors (§3.1):
//
//   FAIL_SPIKE        — 7-day daily-counter baseline (works day one: asynq's
//                       daily counters pre-exist in Redis for 90 days)
//   DEPTH_ANOMALY     — 24h rollup-ring pending baseline (LEARNING until the
//                       series substrate has existed ≥24h)
//   CONSUMERS_DROPPED — 30m consumer-count window, seeded from the hot ring
//                       so lease handovers keep continuity (LEARNING for the
//                       first 30m of series sampling)
//
// The refresher is holder-only state owned by the Engine (like the attention
// debounce map). All Redis reads are budgeted and cadenced:
//   - fail baselines: 7 GETs per queue, once per UTC day (and for new queues)
//   - depth baselines: 1 GET per queue (rollup pending ring), once per hour
//   - consumer seed: 1 GET per queue (hot consumer ring), once per holder
//   - learning anchor: 1 GET (asynqmon:series:since), once per holder
// Per-sweep steady-state cost is zero reads.
//
// Tiers 2-3 additionally cap the per-sweep spend at cmdCap commands (§5.1
// auxiliary budget reserve, phase-12 scale validation): "once per UTC day"
// means EVERY queue's fail baseline goes stale on the same tick at midnight —
// at 5,000 queues that was a single 35,000-command burst, 17× the default
// tick budget (docs/SCALE.md). Capped loads simply resume on the next sweep
// (stale/pending queues remain queued), trading a few minutes of LEARNING
// state on freshly-discovered queues for a bounded per-tick spend. Tier 1
// keeps the uncapped pre-phase-12 behavior — the budget does not govern it.
// ****************************************************************************

const (
	// failBaselineDays is the FAIL_SPIKE lookback (§3.1: "7-day same-hour
	// baseline"). Daily counters carry day totals, so the same-hour value is
	// the day-total average prorated to the current time of day.
	failBaselineDays = 7

	// consumerDropWindow is the CONSUMERS_DROPPED lookback (§3.1: "6→2
	// within 30m").
	consumerDropWindow = 30 * time.Minute

	// consumerCoverageSlack: a queue's window counts as covered once its
	// oldest retained observation is at least window−slack old, so a finding
	// can fire a few sweeps before the literal 30-minute mark instead of
	// flapping on boundary jitter.
	consumerCoverageSlack = 5 * time.Minute

	// depthBaselineWindow / depthMinSamples: DEPTH_ANOMALY compares pending
	// against the mean of the last 24h of rollup samples and requires enough
	// of them that one weird hour cannot define "normal".
	depthBaselineWindow = 24
	depthMinSamples     = 12

	// depthLearningHorizon / consumerLearningHorizon gate the detectors until
	// the series substrate has existed long enough (anchored on the
	// asynqmon:series:since key, which survives restarts and handovers).
	depthLearningHorizon    = 24 * time.Hour
	consumerLearningHorizon = 30 * time.Minute
)

// failBaseline is one queue's 7-day failure baseline.
type failBaseline struct {
	avgDaily float64 // mean of the days that had a counter
	days     int     // how many of the 7 days had one (0 = no baseline)
}

// depthBaseline is one queue's 24h pending-depth baseline.
type depthBaseline struct {
	mean    float64
	samples int
	ok      bool // enough history to judge against (≥24h ring + ≥depthMinSamples)
}

// consumerObsPoint is one retained consumer-count observation. The window
// stores change-points only (plus one boundary point) — max-over-window and
// coverage need nothing denser.
type consumerObsPoint struct {
	t time.Time
	n int
}

// consumerDropObs is the per-queue input the CONSUMERS_DROPPED detector
// consumes: the highest consumer count seen in the last 30m vs now.
type consumerDropObs struct {
	max     int
	current int
	covered bool // window actually spans (window − slack)
}

// detectorInputs is everything the phase-10 detectors need, computed by the
// refresher and passed into the (pure) attention evaluation.
type detectorInputs struct {
	failBaselines  map[string]failBaseline
	depthBaselines map[string]depthBaseline
	consumerDrops  map[string]consumerDropObs

	// Zero = detector live; nonzero = learning until then (§3.1 learning
	// badges: "learning baseline — active in ~Xh").
	depthLearningUntil    time.Time
	consumerLearningUntil time.Time
}

type failCacheEntry struct {
	date string // UTC date the baseline was computed on
	base failBaseline
}

type baselineRefresher struct {
	rc redis.UniversalClient

	// cmdCap bounds the Redis commands one refresh() may issue in tiers 2-3
	// (derived from the §5.1 auxiliary budget reserve by NewEngine; 0 =
	// unlimited). Tier 1 always runs uncapped.
	cmdCap int

	since            time.Time // decoded asynqmon:series:since anchor
	sinceProbed      bool
	failCache        map[string]failCacheEntry
	depthCache       map[string]depthBaseline
	lastDepthRefresh time.Time
	depthPending     []string // queues awaiting this hour's capped depth reload
	consumerSeeded   bool
	windows          map[string][]consumerObsPoint
}

func newBaselineRefresher(rc redis.UniversalClient, cmdCap int) *baselineRefresher {
	return &baselineRefresher{
		rc:         rc,
		cmdCap:     cmdCap,
		failCache:  make(map[string]failCacheEntry),
		depthCache: make(map[string]depthBaseline),
		windows:    make(map[string][]consumerObsPoint),
	}
}

// capFor returns this sweep's command cap: 0 (unlimited) in tier 1, cmdCap
// in tiers 2-3.
func (b *baselineRefresher) capFor(tier int) int {
	if tier <= 1 {
		return 0
	}
	return b.cmdCap
}

// refresh runs once per sweep AFTER the series sampler (so the since anchor
// exists). tier is the sweep plan's §5.1 tier: tiers 2-3 cap the per-sweep
// spend (see the file header). Returns the number of Redis read commands
// issued.
func (b *baselineRefresher) refresh(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, tier int) (int, error) {
	reads := 0
	capCmds := b.capFor(tier)

	if !b.sinceProbed {
		v, err := b.rc.Get(ctx, seriesSinceKey).Int64()
		reads++
		if err != nil && err != redis.Nil {
			return reads, fmt.Errorf("reading series since anchor: %w", err)
		}
		if err == nil && v > 0 {
			b.since = time.Unix(v, 0)
		}
		b.sinceProbed = true
	}

	n, err := b.refreshFailBaselines(ctx, now, snaps, capCmds)
	reads += n
	if err != nil {
		return reads, err
	}
	n, err = b.refreshDepthBaselines(ctx, now, snaps, capCmds)
	reads += n
	if err != nil {
		return reads, err
	}
	n, err = b.seedConsumerWindows(ctx, now, snaps, capCmds)
	reads += n
	if err != nil {
		return reads, err
	}
	b.observeConsumers(now, snaps)
	return reads, nil
}

// refreshFailBaselines loads the previous 7 days' failed counters for queues
// whose cached baseline is missing or from a previous UTC day. Baselines only
// change at UTC midnight, so steady-state cost is zero. capCmds > 0 bounds
// this sweep's spend: the stale list is processed in sorted order, at most
// capCmds/7 queues per sweep — loaded queues leave the stale set, so capped
// sweeps march through the remainder deterministically (a not-yet-loaded
// queue's FAIL_SPIKE detector simply stays silent until its turn).
func (b *baselineRefresher) refreshFailBaselines(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, capCmds int) (int, error) {
	today := now.UTC().Format("2006-01-02")
	var stale []string
	for q := range snaps {
		if e, ok := b.failCache[q]; !ok || e.date != today {
			stale = append(stale, q)
		}
	}
	for q := range b.failCache {
		if _, ok := snaps[q]; !ok {
			delete(b.failCache, q)
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	sort.Strings(stale)
	if maxQueues := capCmds / failBaselineDays; capCmds > 0 && len(stale) > maxQueues {
		if maxQueues < 1 {
			maxQueues = 1
		}
		stale = stale[:maxQueues]
	}

	reads := 0
	pipe := b.rc.Pipeline()
	cmds := make(map[string][]*redis.StringCmd, len(stale))
	for _, q := range stale {
		day := make([]*redis.StringCmd, failBaselineDays)
		for i := 1; i <= failBaselineDays; i++ {
			day[i-1] = pipe.Get(ctx, failedKey(q, now.UTC().AddDate(0, 0, -i)))
			reads++
		}
		cmds[q] = day
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return reads, fmt.Errorf("reading fail baselines: %w", err)
	}
	for _, q := range stale {
		var sum int64
		days := 0
		for _, cmd := range cmds[q] {
			v, err := cmd.Result()
			if err != nil {
				continue // day absent: the queue did not exist / no failures recorded
			}
			sum += decodeInt(v)
			days++
		}
		base := failBaseline{days: days}
		if days > 0 {
			base.avgDaily = float64(sum) / float64(days)
		}
		b.failCache[q] = failCacheEntry{date: today, base: base}
	}
	return reads, nil
}

// refreshDepthBaselines re-reads every queue's rollup pending ring once per
// hour (the ring only gains a slot per hour) and computes the 24h mean. The
// hourly pass enqueues every current queue and drains at most capCmds GETs
// per sweep (capCmds > 0): re-reading 5,000 rings in one tick was a 2.5×
// budget burst (docs/SCALE.md). Results merge into the cache as they load;
// queues gone from the fleet are pruned at each hourly rebuild.
func (b *baselineRefresher) refreshDepthBaselines(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, capCmds int) (int, error) {
	if b.lastDepthRefresh.IsZero() || now.Sub(b.lastDepthRefresh) >= time.Hour {
		b.lastDepthRefresh = now
		b.depthPending = b.depthPending[:0]
		for q := range snaps {
			b.depthPending = append(b.depthPending, q)
		}
		sort.Strings(b.depthPending)
		for q := range b.depthCache {
			if _, ok := snaps[q]; !ok {
				delete(b.depthCache, q)
			}
		}
	}
	if len(b.depthPending) == 0 {
		return 0, nil
	}
	names := b.depthPending
	if capCmds > 0 && len(names) > capCmds {
		names = names[:capCmds]
	}
	b.depthPending = b.depthPending[len(names):]

	reads := 0
	pipe := b.rc.Pipeline()
	cmds := make([]*redis.StringCmd, len(names))
	for i, q := range names {
		cmds[i] = pipe.Get(ctx, seriesKey(rollupRing, seriesKeyScopeQueue(q), MetricPending))
		reads++
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return reads, fmt.Errorf("reading depth baselines: %w", err)
	}
	for i, q := range names {
		raw, _ := cmds[i].Bytes()
		b.depthCache[q] = computeDepthBaseline(raw, now)
	}
	return reads, nil
}

// computeDepthBaseline folds a rollup pending ring into a baseline: the mean
// of the non-null hourly samples in the last 24h, valid only when the ring
// has existed ≥24h and carries enough samples.
func computeDepthBaseline(raw []byte, now time.Time) depthBaseline {
	h := decodeSeriesHeader(raw)
	if !h.ok {
		return depthBaseline{}
	}
	nowP := seriesPeriodIndex(now.Unix(), rollupRing.period)
	var sum int64
	samples := 0
	for p := nowP - depthBaselineWindow; p < nowP; p++ {
		if v := decodeSeriesPoint(raw, h, rollupRing, p); v != nil {
			sum += *v
			samples++
		}
	}
	bl := depthBaseline{samples: samples}
	if samples > 0 {
		bl.mean = float64(sum) / float64(samples)
	}
	firstAt := time.Unix(h.first*rollupRing.period, 0)
	bl.ok = samples >= depthMinSamples && now.Sub(firstAt) >= depthLearningHorizon
	return bl
}

// seedConsumerWindows initializes the 30m consumer windows from the hot
// consumer_count rings once per holder session, so a lease handover does not
// blind the detector for 30 minutes (§3.1 CONSUMERS_DROPPED: "server-level
// series collected from day one"). capCmds > 0 bounds the one-time seed (a
// failover holder inheriting a 5,000-queue cache would otherwise spend 5,000
// GETs in its first sweep); unseeded queues learn organically from
// observeConsumers, exactly like queues discovered after the seed.
func (b *baselineRefresher) seedConsumerWindows(ctx context.Context, now time.Time, snaps map[string]*QueueSnapshot, capCmds int) (int, error) {
	if b.consumerSeeded {
		return 0, nil
	}
	b.consumerSeeded = true

	names := make([]string, 0, len(snaps))
	for q := range snaps {
		names = append(names, q)
	}
	sort.Strings(names)
	if capCmds > 0 && len(names) > capCmds {
		names = names[:capCmds]
	}
	reads := 0
	pipe := b.rc.Pipeline()
	cmds := make([]*redis.StringCmd, len(names))
	for i, q := range names {
		cmds[i] = pipe.Get(ctx, seriesKey(hotRing, seriesKeyScopeQueue(q), MetricConsumerCount))
		reads++
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return reads, fmt.Errorf("seeding consumer windows: %w", err)
	}
	windowSlots := int(consumerDropWindow.Milliseconds() / 1000 / hotRing.period)
	for i, q := range names {
		raw, _ := cmds[i].Bytes()
		h := decodeSeriesHeader(raw)
		if !h.ok {
			continue
		}
		nowP := seriesPeriodIndex(now.Unix(), hotRing.period)
		var win []consumerObsPoint
		for p := nowP - int64(windowSlots); p <= nowP; p++ {
			if v := decodeSeriesPoint(raw, h, hotRing, p); v != nil {
				win = appendConsumerObs(win, time.Unix(p*hotRing.period, 0), int(*v))
			}
		}
		if len(win) > 0 {
			b.windows[q] = win
		}
	}
	return reads, nil
}

// observeConsumers appends this sweep's consumer counts to the windows and
// prunes them to the 30m horizon. Pure in-process (unit-tested directly).
func (b *baselineRefresher) observeConsumers(now time.Time, snaps map[string]*QueueSnapshot) {
	for q, s := range snaps {
		b.windows[q] = pruneConsumerWindow(
			appendConsumerObs(b.windows[q], now, s.Consumers), now)
	}
	for q := range b.windows {
		if _, ok := snaps[q]; !ok {
			delete(b.windows, q)
		}
	}
}

// appendConsumerObs stores change-points only: appending the same value as
// the window tail is a no-op (the tail's earlier timestamp already covers it).
func appendConsumerObs(win []consumerObsPoint, t time.Time, n int) []consumerObsPoint {
	if len(win) > 0 && win[len(win)-1].n == n {
		return win
	}
	return append(win, consumerObsPoint{t: t, n: n})
}

// pruneConsumerWindow drops points that fell out of the 30m window — but
// always keeps the newest such point as the boundary observation: its value
// was still current when the window began.
func pruneConsumerWindow(win []consumerObsPoint, now time.Time) []consumerObsPoint {
	cutoff := now.Add(-consumerDropWindow)
	i := 0
	for i+1 < len(win) && !win[i+1].t.After(cutoff) {
		i++
	}
	return win[i:]
}

// inputs assembles the detector inputs for this sweep's attention evaluation.
func (b *baselineRefresher) inputs(now time.Time, snaps map[string]*QueueSnapshot) detectorInputs {
	det := detectorInputs{
		failBaselines:  make(map[string]failBaseline, len(b.failCache)),
		depthBaselines: b.depthCache,
		consumerDrops:  make(map[string]consumerDropObs, len(snaps)),
	}
	for q, e := range b.failCache {
		det.failBaselines[q] = e.base
	}
	for q, s := range snaps {
		win := b.windows[q]
		obs := consumerDropObs{current: s.Consumers}
		for _, p := range win {
			if p.n > obs.max {
				obs.max = p.n
			}
		}
		if s.Consumers > obs.max {
			obs.max = s.Consumers
		}
		if len(win) > 0 && now.Sub(win[0].t) >= consumerDropWindow-consumerCoverageSlack {
			obs.covered = true
		}
		det.consumerDrops[q] = obs
	}

	// Learning stamps, anchored on when series sampling first existed. An
	// unknown anchor (sampler has not run against this Redis yet) reports the
	// full horizon from now — honest, if slightly drifting until the anchor
	// key appears on the next sweep.
	anchor := b.since
	if anchor.IsZero() {
		anchor = now
	}
	if until := anchor.Add(depthLearningHorizon); now.Before(until) {
		det.depthLearningUntil = until
	}
	if until := anchor.Add(consumerLearningHorizon); now.Before(until) {
		det.consumerLearningUntil = until
	}
	return det
}
