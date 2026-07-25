package hygiene

import (
	"context"
	"sort"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Scheduler health (§3.10): GONE entries (snapshot with no live counterpart,
// §5.12), retention-less entries (outcome unknowable — §3.8), and entries
// targeting zero-consumer queues (live entries × the Servers() coverage
// join).
//
// Inputs are all existing substrates: Inspector.SchedulerEntries() +
// Inspector.Servers() (both O(entries)/O(servers)) and
// stats.ReadSchedulerSnapshots. No per-queue Redis reads.
// ****************************************************************************

// SchedIssue is one flagged scheduler entry.
type SchedIssue struct {
	StableKey string `json:"stable_key"`
	TaskType  string `json:"task_type"`
	Spec      string `json:"spec"`
	Queue     string `json:"queue"`
	// LastSeenAt is set on GONE issues: when the entry was last observed live.
	LastSeenAt time.Time `json:"last_seen_at,omitempty"`
	// RetentionSeconds is the entry's asynq.Retention (0 = completions are
	// deleted immediately; outcomes are unknowable).
	RetentionSeconds int64 `json:"retention_seconds"`
	// Consumers is the target queue's live consumer count.
	Consumers int `json:"consumers"`
}

// SchedulerHealthReport is the scheduler-health payload.
type SchedulerHealthReport struct {
	Gone                []SchedIssue `json:"gone"`
	RetentionLess       []SchedIssue `json:"retention_less"`
	ZeroConsumerTargets []SchedIssue `json:"zero_consumer_targets"`
	LiveEntries         int          `json:"live_entries"`
	Snapshots           int          `json:"snapshots"`
}

// liveSchedEntry is one live scheduler entry in snapshot shape.
type liveSchedEntry struct {
	stableKey string
	entry     stats.SchedulerEntryData
}

// schedHealthInputs are buildSchedulerHealth's pure inputs.
type schedHealthInputs struct {
	now       time.Time
	goneAfter time.Duration
	live      []liveSchedEntry
	snapshots []*stats.SchedulerSnapshot
	consumers map[string]int // queue → live consumer count (Servers() join)
}

// buildSchedulerHealth assembles the report. Pure: no I/O, fully
// unit-testable.
func buildSchedulerHealth(in schedHealthInputs) *SchedulerHealthReport {
	rep := &SchedulerHealthReport{
		Gone:                make([]SchedIssue, 0),
		RetentionLess:       make([]SchedIssue, 0),
		ZeroConsumerTargets: make([]SchedIssue, 0),
		LiveEntries:         len(in.live),
		Snapshots:           len(in.snapshots),
	}

	liveByKey := make(map[string]bool, len(in.live))
	for _, le := range in.live {
		liveByKey[le.stableKey] = true
	}

	// GONE: snapshot, no live counterpart, silent past the gone threshold
	// (same determination as §5.12 — time-gated, no debounce needed).
	for _, snap := range in.snapshots {
		if liveByKey[snap.StableKey] {
			continue
		}
		if in.now.Sub(snap.LastSeen) > in.goneAfter {
			rep.Gone = append(rep.Gone, SchedIssue{
				StableKey:        snap.StableKey,
				TaskType:         snap.Entry.TaskType,
				Spec:             snap.Entry.Spec,
				Queue:            snap.Entry.Queue,
				LastSeenAt:       snap.LastSeen,
				RetentionSeconds: snap.Entry.RetentionSec,
				Consumers:        in.consumers[snap.Entry.Queue],
			})
		}
	}

	// Retention-less and zero-consumer-target: LIVE entries only (a gone
	// scheduler is already its own finding above).
	for _, le := range in.live {
		issue := SchedIssue{
			StableKey:        le.stableKey,
			TaskType:         le.entry.TaskType,
			Spec:             le.entry.Spec,
			Queue:            le.entry.Queue,
			RetentionSeconds: le.entry.RetentionSec,
			Consumers:        in.consumers[le.entry.Queue],
		}
		if le.entry.RetentionSec == 0 {
			rep.RetentionLess = append(rep.RetentionLess, issue)
		}
		if in.consumers[le.entry.Queue] == 0 {
			rep.ZeroConsumerTargets = append(rep.ZeroConsumerTargets, issue)
		}
	}

	byTypeKey := func(s []SchedIssue) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].TaskType != s[j].TaskType {
				return s[i].TaskType < s[j].TaskType
			}
			return s[i].StableKey < s[j].StableKey
		}
	}
	sort.Slice(rep.Gone, byTypeKey(rep.Gone))
	sort.Slice(rep.RetentionLess, byTypeKey(rep.RetentionLess))
	sort.Slice(rep.ZeroConsumerTargets, byTypeKey(rep.ZeroConsumerTargets))
	return rep
}

func schedulerHealthSummary(rep *SchedulerHealthReport) map[string]int64 {
	return map[string]int64{
		"gone":                  int64(len(rep.Gone)),
		"retention_less":        int64(len(rep.RetentionLess)),
		"zero_consumer_targets": int64(len(rep.ZeroConsumerTargets)),
		"live_entries":          int64(rep.LiveEntries),
	}
}

// ----------------------------------------------------------------------------
// Gather.
// ----------------------------------------------------------------------------

// gatherSchedulerHealth produces the report from live substrates. It needs no
// stats snapshot cache — consumer counts come straight from Servers() — so it
// works even with the stats engine disabled.
func gatherSchedulerHealth(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, now time.Time, goneAfter time.Duration) (*SchedulerHealthReport, error) {
	entries, err := insp.SchedulerEntries()
	if err != nil {
		return nil, err
	}
	live := make([]liveSchedEntry, 0, len(entries))
	for _, e := range entries {
		live = append(live, liveSchedEntry{
			stableKey: stats.StableKeyForAsynqEntry(e),
			entry:     stats.SchedulerEntryDataFromAsynq(e),
		})
	}

	servers, err := insp.Servers()
	if err != nil {
		return nil, err
	}
	consumers := make(map[string]int)
	for _, s := range servers {
		for q := range s.Queues {
			consumers[q]++
		}
	}

	snaps, err := stats.ReadSchedulerSnapshots(ctx, rc)
	if err != nil {
		return nil, err
	}
	return buildSchedulerHealth(schedHealthInputs{
		now:       now,
		goneAfter: goneAfter,
		live:      live,
		snapshots: snaps,
		consumers: consumers,
	}), nil
}
