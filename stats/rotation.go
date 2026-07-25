package stats

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the phase-12 command-budget governor (§5.1 tiers 2-3):
//   - tier classification by fleet size (the normative §5.1 tiering table)
//   - the per-tick sweep plan: hot set (refreshed every tick) + rotating cold
//     shard sized so each tick spends ≤ CommandBudget × Interval commands
//   - hot-set selection: attention-flagged ∪ currently-viewed ∪ top-K by
//     last-known backlog/error-rate
//   - ViewTracker: the "currently-viewed queues" bridge — serving replicas
//     record queue views in a shared Redis zset (TTL'd) so the sweeping
//     replica can fold them into the hot set
//
// All planning is pure (planSweep + hotSet + hotScore) and unit-tested with
// an injected clock; the engine only feeds it observed state.
// ****************************************************************************

// Tier boundaries (normative §5.1 tiering table).
const (
	// Tier1MaxQueues: fleets at or below this size get a full sweep every
	// tick — the §5.1 table fixes tier-1 behavior by fleet size; the command
	// budget governs tiers 2-3 only.
	Tier1MaxQueues = 2000
	// Tier2MaxQueues: above this the fleet is tier 3 (two-class: bigger hot
	// set, cold rotation over minutes-to-hours).
	Tier2MaxQueues = 20000

	// DefaultCommandBudget is the §5.1 default: 2,000 commands/second.
	DefaultCommandBudget = 2000

	// Default hot-set K per tier (§5.1: K=500 in tier 2, K=1,000 in tier 3).
	defaultHotSetKTier2 = 500
	defaultHotSetKTier3 = 1000

	// perQueueSweepCost is the budgeted command cost of one queue's hot read:
	// the 15 pipelined commands of sweepBatch plus the amortized
	// pending_since second hop (§5.1's "~14-15 commands/queue", costed
	// pessimistically so the budget is honored even when every queue has
	// pending work).
	perQueueSweepCost = 16

	// perQueueCacheWriteCost is the cache-publish cost of one refreshed row
	// (HSET + PEXPIRE). Scale validation (docs/SCALE.md) showed that sizing
	// shards by the read cost alone systematically overspends the budget by
	// ~12% before any burst work is added, so the planner budgets the FULL
	// per-queue tick cost.
	perQueueCacheWriteCost = 2

	// perQueueTickCost is the total budgeted spend of refreshing one queue in
	// one tick: hot read + cache publish.
	perQueueTickCost = perQueueSweepCost + perQueueCacheWriteCost

	// auxBudgetDiv reserves 1/auxBudgetDiv of each tick's budget for the
	// sweep's auxiliary spend — series-slot flushes (bursty: every hot-ring
	// key flushes when the clock crosses a slot boundary), cadenced baseline
	// loads, the bounded GROUP_STALL reads and the fixed per-sweep overhead
	// (queue-set/index/viewed reads, INFO memory, scheduler index). Without
	// the reserve those bursts land on top of a fully-spent refresh budget
	// (scale validation measured slot-boundary ticks at ~1.6× budget). The
	// auxiliary consumers are themselves capped against this reserve
	// (Engine caps: series flush cap, baseline command cap).
	auxBudgetDiv = 4

	// viewedWindow is how recently a queue must have been requested to count
	// as "currently viewed" (documented approximation: views are recorded
	// best-effort by whichever replica served the request, with a per-queue
	// republish throttle, so the hot set may lag a view by up to
	// viewedRepublishAfter and survive it by up to viewedWindow).
	viewedWindow = 60 * time.Second

	// viewedRepublishAfter throttles per-queue view publishing.
	viewedRepublishAfter = 15 * time.Second

	// viewedKeyTTL keeps the viewed zset from outliving an idle console.
	viewedKeyTTL = 5 * time.Minute
)

// tickCommandBudgets splits one tick's total command allowance
// (budget × interval) into the auxiliary reserve and the refresh budget the
// shard planner may spend on per-queue reads+writes. Pure; shared by the
// planner and by NewEngine when deriving the auxiliary caps.
func tickCommandBudgets(budget int, interval time.Duration) (perTick, aux, refresh int) {
	if budget <= 0 {
		budget = DefaultCommandBudget
	}
	perTick = int(float64(budget) * interval.Seconds())
	if perTick < 1 {
		perTick = 1
	}
	aux = perTick / auxBudgetDiv
	refresh = perTick - aux
	return perTick, aux, refresh
}

// TierForFleet classifies a fleet size into the §5.1 tier (1, 2 or 3).
func TierForFleet(n int) int {
	switch {
	case n <= Tier1MaxQueues:
		return 1
	case n <= Tier2MaxQueues:
		return 2
	default:
		return 3
	}
}

// defaultHotSetK returns the tier's default K when the operator did not set
// one (Config.HotSetK == 0).
func defaultHotSetK(tier int) int {
	if tier >= 3 {
		return defaultHotSetKTier3
	}
	return defaultHotSetKTier2
}

// hotScore ranks a queue's claim to the hot set: last-known backlog
// (pending+active+retry) scaled up by the error rate, plus the error rate
// itself so a failing-but-drained queue stays warm. Pure; deterministic.
func hotScore(s *QueueSnapshot) float64 {
	backlog := float64(s.Pending + s.Active + s.Retry)
	return backlog*(1+s.ErrorRate()) + s.ErrorRate()
}

// hotSet selects the queues refreshed every tick (§5.1 tiers 2-3):
// currently-viewed ∪ attention-flagged ∪ top-K by hotScore over the
// last-known snapshots, bounded at maxHot members (maxHot <= 0 = unbounded).
//
// The ceiling is the scale-validation fix (docs/SCALE.md) for mass
// incidents: a fleet-wide event (every queue NO_CONSUMERS, say) must not
// inflate the hot set until the cold rotation starves at its minimum
// quantum. Members join in priority order — viewed first (a human is
// looking at that queue right now), then attention in the report's
// severity-ranked order, then top-K by score — and queues that miss the
// ceiling simply stay on their rotation cadence with honest RefreshedAt
// stamps. Only names present in the current queue universe are returned,
// sorted. Queues never observed yet cannot be scored and are handled by the
// cold rotation's unseen-first rule instead.
func hotSet(universe map[string]bool, prev map[string]*QueueSnapshot, attention, viewed []string, k, maxHot int) []string {
	set := make(map[string]bool)
	room := func() bool { return maxHot <= 0 || len(set) < maxHot }
	for _, q := range viewed {
		if !room() {
			break
		}
		if universe[q] {
			set[q] = true
		}
	}
	for _, q := range attention {
		if !room() {
			break
		}
		if universe[q] {
			set[q] = true
		}
	}
	// Top-K by score fills the remaining room. Only scored (previously
	// observed) queues compete.
	type scored struct {
		q string
		v float64
	}
	candidates := make([]scored, 0, len(prev))
	for q, s := range prev {
		if universe[q] {
			candidates = append(candidates, scored{q: q, v: hotScore(s)})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].v != candidates[j].v {
			return candidates[i].v > candidates[j].v
		}
		return candidates[i].q < candidates[j].q
	})
	taken := 0
	for _, c := range candidates {
		if taken >= k || !room() {
			break
		}
		if !set[c.q] {
			set[c.q] = true
		}
		taken++
	}
	out := make([]string, 0, len(set))
	for q := range set {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// sweepPlan is one tick's refresh decision.
type sweepPlan struct {
	tier int
	// full: refresh every queue this tick (tier 1, or a budget generous
	// enough that the rotation shard covers the whole cold set).
	full bool
	// hot queues are refreshed every tick regardless of budget (§5.1).
	hot []string
	// cold is this tick's rotation shard (never-observed queues first, then
	// the cursor rotation over the rest).
	cold []string
	// cursor is the rotation position to persist for the next tick.
	cursor int
	// fullRotationEstSec estimates how long a full pass over the cold set
	// takes at this budget (seconds); 0 in full mode.
	fullRotationEstSec int
	// hotSetSize mirrors len(hot) for the coverage stamp.
	hotSetSize int
}

// refreshSet returns the union of hot and cold as a lookup set.
func (p *sweepPlan) refreshSet() map[string]bool {
	m := make(map[string]bool, len(p.hot)+len(p.cold))
	for _, q := range p.hot {
		m[q] = true
	}
	for _, q := range p.cold {
		m[q] = true
	}
	return m
}

// refreshList returns hot ∪ cold, sorted (the sweep batches in name order).
func (p *sweepPlan) refreshList() []string {
	m := p.refreshSet()
	out := make([]string, 0, len(m))
	for q := range m {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

// planSweep computes one tick's plan. Inputs:
//
//	tier      — the fleet's §5.1 tier (the engine computes it, honoring the
//	            in-package test overrides)
//	qnames    — the sorted current queue universe (SMEMBERS asynq:queues)
//	prev      — last-known snapshots (the holder's previous merged publish)
//	attention — queue names flagged by the last attention report
//	viewed    — currently-viewed queue names (ViewTracker)
//	cursor    — the rotation position persisted from the previous tick
//	budget    — commands/second (Config.CommandBudget)
//	interval  — sweep interval (Config.Interval)
//	hotK      — Config.HotSetK (0 = tier default)
//
// Tier 1 (≤ Tier1MaxQueues): full sweep, hot set empty — the §5.1 table fixes
// tier-1 behavior by fleet size; the budget governs tiers 2-3.
//
// Tiers 2-3: the hot set is refreshed every tick regardless of cost (§5.1
// "refreshed every tick regardless" — a hot set larger than the whole budget
// overruns it, visibly, rather than silently dropping hot queues). The
// remaining per-tick budget buys a cold shard: never-observed queues first
// (a new queue in a big fleet becomes visible on the next tick, not after a
// full rotation), then a wrapping cursor rotation over the rest, so every
// queue is refreshed within ~N×cost/budget seconds.
//
// Budget accounting (scale-validated, docs/SCALE.md): the shard is sized
// against the refresh budget — budget×interval minus the 25% auxiliary
// reserve (series flushes, baseline loads, fixed overhead) — at the FULL
// per-queue tick cost (16-command hot read + 2-command cache publish). The
// hot set is bounded at half the refresh budget so the cold rotation always
// keeps at least half: unbounded, the tier default K=500 at interval 1s
// alone would spend 4× the default budget every tick, and a mass incident
// (fleet-wide NO_CONSUMERS) would starve the rotation at its minimum
// quantum. An EXPLICIT Config.HotSetK raises the ceiling to at least K —
// the documented visible-overrun escape hatch.
func planSweep(tier int, qnames []string, prev map[string]*QueueSnapshot, attention, viewed []string, cursor int, budget int, interval time.Duration, hotK int) *sweepPlan {
	n := len(qnames)
	if tier <= 1 {
		return &sweepPlan{tier: 1, full: true, cursor: 0}
	}
	_, _, refreshBudget := tickCommandBudgets(budget, interval)
	maxHot := refreshBudget / (2 * perQueueTickCost)
	if maxHot < 1 {
		maxHot = 1
	}
	if hotK <= 0 {
		hotK = defaultHotSetK(tier)
	} else if hotK > maxHot {
		// The operator explicitly asked for a bigger scored hot set: honor
		// it, visibly overrunning the budget rather than silently shrinking.
		maxHot = hotK
	}

	universe := make(map[string]bool, n)
	for _, q := range qnames {
		universe[q] = true
	}
	hot := hotSet(universe, prev, attention, viewed, hotK, maxHot)
	hotLookup := make(map[string]bool, len(hot))
	for _, q := range hot {
		hotLookup[q] = true
	}

	// Cold universe: everything not hot, split into never-observed (priority)
	// and observed (rotation), both in sorted order.
	var unseen, seen []string
	for _, q := range qnames {
		if hotLookup[q] {
			continue
		}
		if _, ok := prev[q]; ok {
			seen = append(seen, q)
		} else {
			unseen = append(unseen, q)
		}
	}
	coldTotal := len(unseen) + len(seen)

	// Refresh budget available this tick, minus the hot set's fixed cost.
	shardSize := (refreshBudget - len(hot)*perQueueTickCost) / perQueueTickCost
	if shardSize < 1 {
		// A hot set at (or beyond) the whole budget still leaves the cold
		// rotation a minimum quantum of one queue per tick — otherwise cold
		// queues would never refresh at all.
		shardSize = 1
	}

	if shardSize >= coldTotal {
		// The budget covers everything: rotation degenerates to a full sweep.
		return &sweepPlan{
			tier:               tier,
			full:               true,
			hot:                hot,
			cursor:             0,
			hotSetSize:         len(hot),
			fullRotationEstSec: int(interval.Seconds()),
		}
	}

	cold := make([]string, 0, shardSize)
	cold = append(cold, unseen...)
	if len(cold) > shardSize {
		cold = cold[:shardSize]
	}
	newCursor := cursor
	if remaining := shardSize - len(cold); remaining > 0 && len(seen) > 0 {
		start := cursor % len(seen)
		for i := 0; i < remaining && i < len(seen); i++ {
			cold = append(cold, seen[(start+i)%len(seen)])
		}
		taken := remaining
		if taken > len(seen) {
			taken = len(seen)
		}
		newCursor = (start + taken) % len(seen)
	}

	// Full-rotation estimate: ticks to cover the cold set at this shard size.
	ticks := (coldTotal + shardSize - 1) / shardSize
	return &sweepPlan{
		tier:               tier,
		hot:                hot,
		cold:               cold,
		cursor:             newCursor,
		hotSetSize:         len(hot),
		fullRotationEstSec: int(float64(ticks) * interval.Seconds()),
	}
}

// ----------------------------------------------------------------------------
// ViewTracker — the "currently viewed" bridge (§5.1 hot set input).
// ----------------------------------------------------------------------------

// ViewTracker records queue views on the serving replica and publishes them
// to a small shared Redis zset (score = view time, TTL'd) so the sweeping
// replica folds recently-viewed queues into the hot set.
//
// Documented approximation: publishing is best-effort and throttled per
// queue (viewedRepublishAfter), the window is viewedWindow, and a failed
// publish is dropped silently — a missed view only means one queue refreshes
// on its rotation cadence instead of every tick. Correctness (coverage
// stamps, staleness dots) never depends on this signal.
type ViewTracker struct {
	rc  redis.UniversalClient
	now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// NewViewTracker creates a ViewTracker. now == nil uses time.Now.
func NewViewTracker(rc redis.UniversalClient, now func() time.Time) *ViewTracker {
	if now == nil {
		now = time.Now
	}
	return &ViewTracker{rc: rc, now: now, last: make(map[string]time.Time)}
}

// MarkViewed records that a queue was requested (directory name= filter or a
// workspace endpoint). Throttled per queue; best-effort (errors ignored).
func (v *ViewTracker) MarkViewed(ctx context.Context, qname string) {
	if v == nil || qname == "" {
		return
	}
	now := v.now()
	v.mu.Lock()
	if last, ok := v.last[qname]; ok && now.Sub(last) < viewedRepublishAfter {
		v.mu.Unlock()
		return
	}
	v.last[qname] = now
	// Opportunistic in-process GC so the throttle map cannot grow unbounded.
	if len(v.last) > 4096 {
		for q, at := range v.last {
			if now.Sub(at) > viewedWindow {
				delete(v.last, q)
			}
		}
	}
	v.mu.Unlock()

	pipe := v.rc.Pipeline()
	pipe.ZAdd(ctx, viewedKey, redis.Z{Score: float64(now.Unix()), Member: qname})
	pipe.PExpire(ctx, viewedKey, viewedKeyTTL)
	_, _ = pipe.Exec(ctx) // best-effort by design
}

// readViewed returns queues viewed within viewedWindow and prunes expired
// members. Called by the sweeping holder in tiers 2-3 only (2 commands).
func readViewed(ctx context.Context, rc redis.UniversalClient, now time.Time) ([]string, int, error) {
	cutoff := strconv.FormatInt(now.Add(-viewedWindow).Unix(), 10)
	pipe := rc.Pipeline()
	pipe.ZRemRangeByScore(ctx, viewedKey, "-inf", "("+cutoff)
	rangeCmd := pipe.ZRangeByScore(ctx, viewedKey, &redis.ZRangeBy{Min: cutoff, Max: "+inf"})
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, 2, err
	}
	names, _ := rangeCmd.Result()
	return names, 2, nil
}
