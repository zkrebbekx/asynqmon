package hygiene

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Queue inventory (§3.10): per queue — last-activity evidence, dead-queue
// candidates (zero size ∧ zero consumers ∧ no counter movement 30d), paused
// >7d, zero-consumer with pending.
//
// buildInventory is a pure function over the stats snapshots plus the bounded
// movement probes; gatherInventory performs the probes:
//   - series rollup first (2 pipelined GETs per candidate via
//     stats.ReadSeriesBatch) when the ring covers the full 30d window;
//   - else one MGET of the 60 daily counter keys (30 processed + 30 failed —
//     all in the queue's hash slot, so the MGET is cluster-safe).
// Probes run only for dead-queue CANDIDATES (zero size ∧ zero consumers ∧ no
// counter movement today), capped at inventoryProbeCap per run — the report
// says so when the cap bites instead of silently under-reporting.
// ****************************************************************************

const (
	// inventoryWindow is the dead-queue silence window (§3.10: "no counter
	// movement 30d") — also the §5.8 rollup ring's exact span.
	inventoryWindowDays = 30

	// pausedLongAfter mirrors the PAUSED_LONG detector threshold (§3.10
	// "paused >7d").
	pausedLongAfter = 7 * 24 * time.Hour

	// inventoryProbeCap bounds the per-run movement probes.
	inventoryProbeCap = 500

	// maxInventoryRows caps the report body; the summary counts always cover
	// the whole fleet.
	maxInventoryRows = 1000

	// seriesSpecBatch bounds one ReadSeriesBatch pipeline.
	seriesSpecBatch = 200
)

// Last-activity evidence sources (frozen frontend contract).
const (
	ActivityActiveNow     = "active_now"     // tasks in the active list at snapshot time
	ActivityCountersToday = "counters_today" // today's processed/failed counters moved
	ActivityOldestPending = "oldest_pending" // oldest waiting task's enqueue time (a lower bound)
	ActivitySeries        = "series"         // last non-zero processed/failed delta in the 30d rollup
	ActivityDailyCounters = "daily_counters" // last daily counter with movement in the 30d window
	ActivityNone          = "none"           // probed: no movement observed in the 30d window
	ActivityUnknown       = "unknown"        // not probed (queue has content; cheap evidence only)
)

// InventoryRow is one queue of the inventory report.
type InventoryRow struct {
	Queue     string `json:"queue"`
	Size      int64  `json:"size"` // tasks across all states
	Pending   int64  `json:"pending"`
	Consumers int    `json:"consumers"`

	Paused      bool      `json:"paused"`
	PausedSince time.Time `json:"paused_since,omitempty"`

	// LastActivityAt is the newest activity evidence found (zero = none);
	// LastActivitySource says which substrate produced it.
	LastActivityAt     time.Time `json:"last_activity_at,omitempty"`
	LastActivitySource string    `json:"last_activity_source"`

	// DeadCandidate: zero size ∧ zero consumers ∧ no counter movement in the
	// 30d window. DeadEvidence names the substrate that proved the silence
	// ("series" or "daily_counters").
	DeadCandidate bool   `json:"dead_candidate"`
	DeadEvidence  string `json:"dead_evidence,omitempty"`

	ZeroConsumerPending bool `json:"zero_consumer_pending"`
	PausedOver7d        bool `json:"paused_over_7d"`
}

// InventoryReport is the queue-inventory payload.
type InventoryReport struct {
	// Rows are flagged-first, then stalest-activity-first (capped).
	Rows          []InventoryRow `json:"rows"`
	QueuesTotal   int            `json:"queues_total"`
	RowsTruncated bool           `json:"rows_truncated"`
	// Probe accounting: how many dead-queue candidates were probed for 30d
	// movement, and whether the cap left some unexamined (those queues stay
	// unflagged — a capped run under-reports candidates, never fabricates).
	ProbeCap     int  `json:"probe_cap"`
	ProbesCapped bool `json:"probes_capped"`
}

// movementProbe is one candidate's 30d movement determination.
type movementProbe struct {
	lastAt time.Time // zero = no movement inside the window
	source string    // ActivitySeries or ActivityDailyCounters (the substrate consulted)
}

// inventoryInputs are buildInventory's pure inputs.
type inventoryInputs struct {
	now          time.Time
	snaps        []*stats.QueueSnapshot
	movement     map[string]movementProbe // probed candidates only
	probesCapped bool
}

// snapshotSize sums a queue's task counts across all states.
func snapshotSize(s *stats.QueueSnapshot) int64 {
	return s.Pending + s.Active + s.Scheduled + s.Retry + s.Archived + s.Completed
}

// isDeadQueueCandidate is the CHEAP part of the dead-queue test — the probe
// then decides using the 30d substrates.
func isDeadQueueCandidate(s *stats.QueueSnapshot) bool {
	return snapshotSize(s) == 0 && s.Consumers == 0 && s.ProcessedToday == 0 && s.FailedToday == 0
}

// buildInventory assembles the report. Pure: no I/O, fully unit-testable.
func buildInventory(in inventoryInputs) *InventoryReport {
	rows := make([]InventoryRow, 0, len(in.snaps))
	for _, s := range in.snaps {
		row := InventoryRow{
			Queue:               s.Queue,
			Size:                snapshotSize(s),
			Pending:             s.Pending,
			Consumers:           s.Consumers,
			Paused:              s.Paused,
			PausedSince:         s.PausedSince,
			ZeroConsumerPending: s.Consumers == 0 && s.Pending > 0,
			PausedOver7d:        s.Paused && !s.PausedSince.IsZero() && in.now.Sub(s.PausedSince) > pausedLongAfter,
		}

		// Last-activity evidence: newest of the cheap snapshot signals, then
		// the probe result for probed queues.
		if s.Active > 0 {
			row.LastActivityAt, row.LastActivitySource = s.RefreshedAt, ActivityActiveNow
		} else if s.ProcessedToday > 0 || s.FailedToday > 0 {
			row.LastActivityAt, row.LastActivitySource = s.RefreshedAt, ActivityCountersToday
		} else if !s.OldestPendingSince.IsZero() {
			row.LastActivityAt, row.LastActivitySource = s.OldestPendingSince, ActivityOldestPending
		}

		if probe, probed := in.movement[s.Queue]; probed {
			switch {
			case !probe.lastAt.IsZero() && probe.lastAt.After(row.LastActivityAt):
				row.LastActivityAt, row.LastActivitySource = probe.lastAt, probe.source
			case probe.lastAt.IsZero() && row.LastActivityAt.IsZero():
				row.LastActivitySource = ActivityNone
			}
			// The probe is authoritative for the dead-queue determination:
			// candidate iff still cheap-eligible and no movement in-window.
			if isDeadQueueCandidate(s) && probe.lastAt.IsZero() {
				row.DeadCandidate = true
				row.DeadEvidence = probe.source
			}
		} else if row.LastActivityAt.IsZero() {
			row.LastActivitySource = ActivityUnknown
		}
		rows = append(rows, row)
	}

	rep := &InventoryReport{
		QueuesTotal:  len(rows),
		ProbeCap:     inventoryProbeCap,
		ProbesCapped: in.probesCapped,
	}

	// Flagged-first, stalest-first, name for determinism.
	flagRank := func(r InventoryRow) int {
		switch {
		case r.DeadCandidate:
			return 0
		case r.ZeroConsumerPending:
			return 1
		case r.PausedOver7d:
			return 2
		}
		return 3
	}
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := flagRank(rows[i]), flagRank(rows[j])
		if ri != rj {
			return ri < rj
		}
		ai, aj := rows[i].LastActivityAt, rows[j].LastActivityAt
		if !ai.Equal(aj) {
			// Zero (no evidence) sorts before any timestamp; else older first.
			if ai.IsZero() != aj.IsZero() {
				return ai.IsZero()
			}
			return ai.Before(aj)
		}
		return rows[i].Queue < rows[j].Queue
	})
	if len(rows) > maxInventoryRows {
		rows = rows[:maxInventoryRows]
		rep.RowsTruncated = true
	}
	rep.Rows = rows
	return rep
}

func inventorySummary(rep *InventoryReport) map[string]int64 {
	sum := map[string]int64{
		"queues_total":          int64(rep.QueuesTotal),
		"dead_queue_candidates": 0,
		"zero_consumer_pending": 0,
		"paused_over_7d":        0,
	}
	for _, r := range rep.Rows {
		if r.DeadCandidate {
			sum["dead_queue_candidates"]++
		}
		if r.ZeroConsumerPending {
			sum["zero_consumer_pending"]++
		}
		if r.PausedOver7d {
			sum["paused_over_7d"]++
		}
	}
	return sum
}

// ----------------------------------------------------------------------------
// Gather (bounded Redis reads).
// ----------------------------------------------------------------------------

// gatherMovementProbes resolves the 30d movement question for each candidate:
// series rollup where it covers the window, daily counters otherwise.
func gatherMovementProbes(ctx context.Context, rc redis.UniversalClient, candidates []string, now time.Time) (map[string]movementProbe, error) {
	out := make(map[string]movementProbe, len(candidates))
	if len(candidates) == 0 {
		return out, nil
	}

	windowStart := now.Add(-inventoryWindowDays * 24 * time.Hour)

	// Pass 1 — series rollups, batched: processed_delta + failed_delta, 30d.
	needCounters := make([]string, 0, len(candidates))
	for start := 0; start < len(candidates); start += seriesSpecBatch / 2 {
		end := start + seriesSpecBatch/2
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[start:end]
		specs := make([]stats.SeriesSpec, 0, len(batch)*2)
		for _, q := range batch {
			scope := stats.SeriesScope{Kind: "queue", Name: q}
			specs = append(specs,
				stats.SeriesSpec{Scope: scope, Metric: stats.MetricProcessedDelta, Window: stats.SeriesWindow30d},
				stats.SeriesSpec{Scope: scope, Metric: stats.MetricFailedDelta, Window: stats.SeriesWindow30d},
			)
		}
		series, err := stats.ReadSeriesBatch(ctx, rc, specs, now)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(batch); i++ {
			q := batch[i]
			covered, lastMove := seriesMovement(series[i*2], windowStart)
			c2, m2 := seriesMovement(series[i*2+1], windowStart)
			covered = covered || c2
			if m2.After(lastMove) {
				lastMove = m2
			}
			if covered {
				out[q] = movementProbe{lastAt: lastMove, source: ActivitySeries}
			} else {
				needCounters = append(needCounters, q)
			}
		}
	}

	// Pass 2 — daily counters for queues the ring cannot answer: one MGET of
	// 60 keys per queue (all keys share the queue's hash slot).
	if len(needCounters) > 0 {
		pipe := rc.Pipeline()
		cmds := make([]*redis.SliceCmd, len(needCounters))
		days := make([]time.Time, inventoryWindowDays)
		for d := 0; d < inventoryWindowDays; d++ {
			days[d] = now.UTC().AddDate(0, 0, -d)
		}
		for i, q := range needCounters {
			keys := make([]string, 0, inventoryWindowDays*2)
			for _, day := range days {
				keys = append(keys, processedKey(q, day), failedKey(q, day))
			}
			cmds[i] = pipe.MGet(ctx, keys...)
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return nil, err
		}
		for i, q := range needCounters {
			vals, err := cmds[i].Result()
			probe := movementProbe{source: ActivityDailyCounters}
			if err == nil {
				// vals[2d]/vals[2d+1] belong to day now-d; find the NEWEST day
				// with movement (smallest d).
				for d := 0; d < inventoryWindowDays && d*2+1 < len(vals); d++ {
					if counterMoved(vals[d*2]) || counterMoved(vals[d*2+1]) {
						day := days[d]
						probe.lastAt = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
						break
					}
				}
			}
			out[q] = probe
		}
	}
	return out, nil
}

// seriesMovement inspects one 30d rollup series: covered reports whether the
// ring's data reaches back to windowStart (its earliest non-null point is at
// or before it), lastAt is the newest point with a value > 0. A ring that
// covers only part of the window cannot prove 30 days of silence — the
// caller then falls back to daily counters.
func seriesMovement(s *stats.Series, windowStart time.Time) (covered bool, lastAt time.Time) {
	var earliest time.Time
	for _, p := range s.Points {
		if p.V == nil {
			continue
		}
		at := time.Unix(p.T, 0)
		if earliest.IsZero() {
			earliest = at
		}
		if *p.V > 0 && at.After(lastAt) {
			lastAt = at
		}
	}
	// The first rollup slot lands up to an hour after sampling starts; one
	// slot of slack keeps a ring that has genuinely run for the whole window
	// from flunking coverage on boundary arithmetic.
	covered = !earliest.IsZero() && !earliest.After(windowStart.Add(time.Hour))
	return covered, lastAt
}

// counterMoved parses one MGET value ("nil" for absent days).
func counterMoved(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0
}

// gatherInventory produces the report from live substrates.
func gatherInventory(ctx context.Context, rc redis.UniversalClient, snaps []*stats.QueueSnapshot, now time.Time) (*InventoryReport, error) {
	var candidates []string
	for _, s := range snaps {
		if isDeadQueueCandidate(s) {
			candidates = append(candidates, s.Queue)
		}
	}
	sort.Strings(candidates)
	capped := false
	if len(candidates) > inventoryProbeCap {
		candidates = candidates[:inventoryProbeCap]
		capped = true
	}
	movement, err := gatherMovementProbes(ctx, rc, candidates, now)
	if err != nil {
		return nil, err
	}
	return buildInventory(inventoryInputs{now: now, snaps: snaps, movement: movement, probesCapped: capped}), nil
}
