package stats

import (
	"strconv"
	"time"
)

// ****************************************************************************
// This file defines:
//   - QueueSnapshot / FleetSnapshot, the units of data the sweeper produces
//   - their encoding to and from Redis hashes (the shared cache format)
// ****************************************************************************

// QueueSnapshot is one queue's stats as observed by a single sweep. All
// counts are point-in-time reads of a live system; RefreshedAt states when.
// The honest-numbers rule (§ principles 2): a snapshot is never served
// without its RefreshedAt, so consumers can always label staleness.
type QueueSnapshot struct {
	Queue string

	// Per-state sizes.
	Pending   int64
	Active    int64
	Scheduled int64
	Retry     int64
	Archived  int64
	Completed int64

	// Groups is the number of aggregation groups (SCARD of the groups set).
	// Aggregating task totals require per-group reads and are deliberately
	// not part of the tier-1 hot sweep.
	Groups int64

	// Paused reports whether the queue's paused marker exists; PausedSince is
	// the marker's value (asynq stores the pause time). Zero when the value
	// is missing or unparsable — existence alone determines Paused.
	Paused      bool
	PausedSince time.Time

	// Daily counters (UTC day of the sweep). Never trimmed by asynq, so these
	// are magnitude truth even when archived sets hit their cap.
	ProcessedToday int64
	FailedToday    int64

	// OrphanCandidates counts lease entries expired beyond the grace period.
	// "Candidates" because asynq's recoverer normally requeues them within
	// seconds; flagging them as real orphans requires debounce across sweeps
	// (attention engine, phase 4).
	OrphanCandidates int64

	// NextRetryAt is the soonest retry fire time (zero when the retry set is
	// empty).
	NextRetryAt time.Time

	// RetryDueSoon counts retry tasks whose next fire time falls within the
	// RETRY_STORM window ([sweep time, +5m]) — the "storm mass" the attention
	// engine compares against its threshold (§3.1 RETRY STORM).
	RetryDueSoon int64

	// PastDueScheduled counts scheduled tasks whose process-at time already
	// passed — nonzero values mean asynq's forwarder is stalled or absent.
	PastDueScheduled int64

	// OldestPendingSince is when the task at the tail of the pending LIST
	// entered pending (zero when the queue has no pending tasks or the task
	// hash vanished between the two pipelined hops). Age derived from this is
	// exactly asynq's QueueInfo.Latency.
	OldestPendingSince time.Time

	// Consumers is the number of live servers whose queue-priority map
	// includes this queue. Derived from Inspector.Servers(), not from queue
	// keys — it cannot be computed from the per-queue sweep alone.
	Consumers int

	// RefreshedAt is when the sweep that produced this snapshot ran.
	RefreshedAt time.Time
}

// ErrorRate returns failed/processed for the current UTC day, in [0, 1].
// Returns 0 when nothing was processed (no data is not a 100% failure rate).
func (s *QueueSnapshot) ErrorRate() float64 {
	if s.ProcessedToday <= 0 {
		return 0
	}
	return float64(s.FailedToday) / float64(s.ProcessedToday)
}

// OldestPendingAge returns how long the oldest pending task has been waiting
// as of now, or 0 when unknown/empty.
func (s *QueueSnapshot) OldestPendingAge(now time.Time) time.Duration {
	if s.OldestPendingSince.IsZero() {
		return 0
	}
	d := now.Sub(s.OldestPendingSince)
	if d < 0 {
		return 0
	}
	return d
}

// FleetSnapshot aggregates one sweep across every swept queue, plus
// server-derived worker totals (Inspector.Servers(), O(servers) per sweep).
type FleetSnapshot struct {
	QueuesTotal        int
	PausedQueues       int
	ZeroConsumerQueues int

	Pending   int64
	Active    int64
	Scheduled int64
	Retry     int64
	Archived  int64
	Completed int64
	Groups    int64

	ProcessedToday int64
	FailedToday    int64

	OrphanCandidates int64
	PastDueScheduled int64

	Servers      int
	WorkersTotal int // sum of server concurrency
	WorkersBusy  int // sum of active workers

	// RedisMemoryUsed / RedisMemoryMax are INFO memory's used_memory and
	// maxmemory, sampled once per sweep for the Fleet landing's Redis tile
	// (§3.1). Max 0 means Redis has no configured maxmemory; Used 0 means
	// the INFO read failed (unknown, not "empty"). On a Redis Cluster the
	// INFO answer comes from a single node — a tier-1 approximation.
	RedisMemoryUsed int64
	RedisMemoryMax  int64

	// Phase-12 governor metadata (§5.1 tiers, §3.12 health readout),
	// persisted in the cache so standby replicas serve the identical
	// coverage stamp: the tier the producing sweep ran in (1|2|3 by fleet
	// size), the configured command budget (cmds/sec), the hot-set size the
	// tick refreshed, and the estimated seconds for a full cold rotation
	// (0 = every queue refreshed every tick).
	Tier               int
	CommandBudget      int
	HotSetSize         int
	FullRotationEstSec int

	RefreshedAt time.Time
}

// ErrorRate returns the fleet-wide failed/processed ratio for the current UTC
// day, in [0, 1]; 0 when nothing was processed.
func (f *FleetSnapshot) ErrorRate() float64 {
	if f.ProcessedToday <= 0 {
		return 0
	}
	return float64(f.FailedToday) / float64(f.ProcessedToday)
}

// ----------------------------------------------------------------------------
// Redis hash encoding. Times are stored as Unix nanoseconds ("0" = zero time)
// so decoding needs no format parsing and zero-values round-trip exactly.
// Field names are part of the replica-to-replica cache contract: bump with
// care, and prefer additive changes (decoders default missing fields to zero).
// ----------------------------------------------------------------------------

func encodeTime(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func decodeTime(v string) time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func decodeInt(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// decodeIntSized parses a cache field destined for a plain int. The cache is
// asynqmon-owned, but it lives in a shared Redis — a corrupted or hostile
// value must degrade to zero, not wrap around in a narrowing conversion.
func decodeIntSized(v string) int {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}
	return int(n)
}

func encodeBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (s *QueueSnapshot) toHash() map[string]interface{} {
	return map[string]interface{}{
		"queue":                s.Queue,
		"pending":              s.Pending,
		"active":               s.Active,
		"scheduled":            s.Scheduled,
		"retry":                s.Retry,
		"archived":             s.Archived,
		"completed":            s.Completed,
		"groups":               s.Groups,
		"paused":               encodeBool(s.Paused),
		"paused_since":         encodeTime(s.PausedSince),
		"processed_today":      s.ProcessedToday,
		"failed_today":         s.FailedToday,
		"orphan_candidates":    s.OrphanCandidates,
		"next_retry_at":        encodeTime(s.NextRetryAt),
		"retry_due_5m":         s.RetryDueSoon,
		"past_due_scheduled":   s.PastDueScheduled,
		"oldest_pending_since": encodeTime(s.OldestPendingSince),
		"consumers":            s.Consumers,
		"refreshed_at":         encodeTime(s.RefreshedAt),
	}
}

func queueSnapshotFromHash(h map[string]string) *QueueSnapshot {
	return &QueueSnapshot{
		Queue:              h["queue"],
		Pending:            decodeInt(h["pending"]),
		Active:             decodeInt(h["active"]),
		Scheduled:          decodeInt(h["scheduled"]),
		Retry:              decodeInt(h["retry"]),
		Archived:           decodeInt(h["archived"]),
		Completed:          decodeInt(h["completed"]),
		Groups:             decodeInt(h["groups"]),
		Paused:             h["paused"] == "1",
		PausedSince:        decodeTime(h["paused_since"]),
		ProcessedToday:     decodeInt(h["processed_today"]),
		FailedToday:        decodeInt(h["failed_today"]),
		OrphanCandidates:   decodeInt(h["orphan_candidates"]),
		NextRetryAt:        decodeTime(h["next_retry_at"]),
		RetryDueSoon:       decodeInt(h["retry_due_5m"]),
		PastDueScheduled:   decodeInt(h["past_due_scheduled"]),
		OldestPendingSince: decodeTime(h["oldest_pending_since"]),
		Consumers:          decodeIntSized(h["consumers"]),
		RefreshedAt:        decodeTime(h["refreshed_at"]),
	}
}

func (f *FleetSnapshot) toHash() map[string]interface{} {
	return map[string]interface{}{
		"queues_total":         f.QueuesTotal,
		"paused_queues":        f.PausedQueues,
		"zero_consumer_queues": f.ZeroConsumerQueues,
		"pending":              f.Pending,
		"active":               f.Active,
		"scheduled":            f.Scheduled,
		"retry":                f.Retry,
		"archived":             f.Archived,
		"completed":            f.Completed,
		"groups":               f.Groups,
		"processed_today":      f.ProcessedToday,
		"failed_today":         f.FailedToday,
		"orphan_candidates":    f.OrphanCandidates,
		"past_due_scheduled":   f.PastDueScheduled,
		"servers":              f.Servers,
		"workers_total":        f.WorkersTotal,
		"workers_busy":         f.WorkersBusy,
		"redis_memory_used":    f.RedisMemoryUsed,
		"redis_memory_max":     f.RedisMemoryMax,
		"tier":                 f.Tier,
		"command_budget":       f.CommandBudget,
		"hot_set_size":         f.HotSetSize,
		"full_rotation_est_s":  f.FullRotationEstSec,
		"refreshed_at":         encodeTime(f.RefreshedAt),
	}
}

func fleetSnapshotFromHash(h map[string]string) *FleetSnapshot {
	return &FleetSnapshot{
		QueuesTotal:        decodeIntSized(h["queues_total"]),
		PausedQueues:       decodeIntSized(h["paused_queues"]),
		ZeroConsumerQueues: decodeIntSized(h["zero_consumer_queues"]),
		Pending:            decodeInt(h["pending"]),
		Active:             decodeInt(h["active"]),
		Scheduled:          decodeInt(h["scheduled"]),
		Retry:              decodeInt(h["retry"]),
		Archived:           decodeInt(h["archived"]),
		Completed:          decodeInt(h["completed"]),
		Groups:             decodeInt(h["groups"]),
		ProcessedToday:     decodeInt(h["processed_today"]),
		FailedToday:        decodeInt(h["failed_today"]),
		OrphanCandidates:   decodeInt(h["orphan_candidates"]),
		PastDueScheduled:   decodeInt(h["past_due_scheduled"]),
		Servers:            decodeIntSized(h["servers"]),
		WorkersTotal:       decodeIntSized(h["workers_total"]),
		WorkersBusy:        decodeIntSized(h["workers_busy"]),
		RedisMemoryUsed:    decodeInt(h["redis_memory_used"]),
		RedisMemoryMax:     decodeInt(h["redis_memory_max"]),
		Tier:               decodeIntSized(h["tier"]),
		CommandBudget:      decodeIntSized(h["command_budget"]),
		HotSetSize:         decodeIntSized(h["hot_set_size"]),
		FullRotationEstSec: decodeIntSized(h["full_rotation_est_s"]),
		RefreshedAt:        decodeTime(h["refreshed_at"]),
	}
}
