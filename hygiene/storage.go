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
// Storage attribution (§3.10/§5.14): memory by queue (sampled, staleness-
// stamped; unsampled queues say "not sampled", never 0), completed-retention
// pressure histogram (expiry-score buckets — when the janitor reclaims), and
// top payload offenders (bounded head/tail sampling of each big queue's
// largest states, labeled "sampled").
//
// Bounds per run: the top storageSampleQueues queues by task count get
//   - 6 MEMORY USAGE (one per state structure)
//   - ≤4 range reads collecting sample IDs from the two largest states
//   - ≤storageSamplePerQueue × 2 commands (MEMORY USAGE + HSTRLEN msg)
// and the top retentionQueueCap queues by completed count get 7 pipelined
// ZCOUNTs (exact). Everything else is served from the stats cache.
// ****************************************************************************

const (
	// storageSampleQueues (K): how many queues get memory-sampled per run.
	storageSampleQueues = 20
	// storageSamplePerQueue (N): task hashes sampled per sampled queue.
	storageSamplePerQueue = 16
	// maxPayloadOffenders caps the offender list.
	maxPayloadOffenders = 20
	// retentionQueueCap bounds the per-queue retention histogram rows.
	retentionQueueCap = 50
)

// storageMethod is the honest sampling label, rendered verbatim by the UI.
const storageMethod = "sampled: MEMORY USAGE over the state structures plus head/tail task probes " +
	"of each queue's two largest states; per-queue totals are structure bytes + avg sampled " +
	"task bytes × task count (estimate). Queues beyond the top-K by task count are not sampled."

// QueueMemory is one sampled queue's memory attribution.
type QueueMemory struct {
	Queue string `json:"queue"`
	Tasks int64  `json:"tasks"`
	// StructBytes is the summed MEMORY USAGE of the six state structures
	// (exact at sample time; task bodies live in separate hashes).
	StructBytes int64 `json:"struct_bytes"`
	// SampledTasks / AvgTaskBytes: the task-hash probe. EstimatedBytes =
	// StructBytes + AvgTaskBytes × Tasks — an estimate, labeled by Method on
	// the report.
	SampledTasks   int       `json:"sampled_tasks"`
	AvgTaskBytes   int64     `json:"avg_task_bytes"`
	EstimatedBytes int64     `json:"estimated_bytes"`
	SampledAt      time.Time `json:"sampled_at"`
}

// PayloadOffender is one sampled task among the largest observed.
type PayloadOffender struct {
	Queue string `json:"queue"`
	ID    string `json:"id"`
	State string `json:"state"`
	// MsgBytes is HSTRLEN of the task hash's "msg" field — the encoded task
	// message (type + payload + options), the honest payload-weight proxy.
	MsgBytes int64 `json:"msg_bytes"`
}

// RetentionRow is one queue's completed-retention histogram (exact ZCOUNTs
// over completed-zset expiry scores).
type RetentionRow struct {
	Queue string `json:"queue"`
	// Counts follow the report's BucketLabels order.
	Counts []int64 `json:"counts"`
	Total  int64   `json:"total"`
}

// RetentionHistogram is the "when does the janitor reclaim" forecast.
type RetentionHistogram struct {
	// BucketLabels: past_due (expiry already behind now — reclaimable the
	// moment a server's janitor looks), then forward horizons.
	BucketLabels    []string       `json:"bucket_labels"`
	Fleet           RetentionRow   `json:"fleet"`
	Queues          []RetentionRow `json:"queues"`
	QueuesTruncated bool           `json:"queues_truncated"`
}

// StorageReport is the storage-attribution payload.
type StorageReport struct {
	// Memory rows exist ONLY for sampled queues — a missing queue means "not
	// sampled", and the UI must say so rather than showing 0.
	Memory           []QueueMemory      `json:"memory"`
	NotSampledQueues int                `json:"not_sampled_queues"`
	SamplePerQueue   int                `json:"sample_per_queue"`
	Method           string             `json:"method"`
	Offenders        []PayloadOffender  `json:"offenders"`
	Retention        RetentionHistogram `json:"retention"`
}

// retentionBucketLabels is the frozen bucket order.
var retentionBucketLabels = []string{"past_due", "<1h", "1-6h", "6-24h", "1-3d", "3-7d", ">7d"}

// retentionBucketRanges returns the [min,max] ZCOUNT bounds (Unix-second
// strings) for each bucket at the given instant. Pure — unit-tested.
func retentionBucketRanges(now time.Time) [][2]string {
	sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	n := now
	return [][2]string{
		{"-inf", sec(n)},                                                   // past_due: expiry ≤ now
		{"(" + sec(n), sec(n.Add(time.Hour))},                              // <1h
		{"(" + sec(n.Add(time.Hour)), sec(n.Add(6 * time.Hour))},           // 1-6h
		{"(" + sec(n.Add(6*time.Hour)), sec(n.Add(24 * time.Hour))},        // 6-24h
		{"(" + sec(n.Add(24*time.Hour)), sec(n.Add(3 * 24 * time.Hour))},   // 1-3d
		{"(" + sec(n.Add(3*24*time.Hour)), sec(n.Add(7 * 24 * time.Hour))}, // 3-7d
		{"(" + sec(n.Add(7*24*time.Hour)), "+inf"},                         // >7d
	}
}

// taskSample is one probed task hash.
type taskSample struct {
	ID       string
	State    string
	MemBytes int64 // MEMORY USAGE of the task hash (0 = probe missed)
	MsgBytes int64 // HSTRLEN of the "msg" field
}

// storageProbe is one queue's gathered raw sampling data.
type storageProbe struct {
	Queue       string
	Tasks       int64
	StructBytes int64
	Samples     []taskSample
	SampledAt   time.Time
}

// storageInputs are buildStorage's pure inputs.
type storageInputs struct {
	snaps          []*stats.QueueSnapshot
	probes         []storageProbe
	retentionRows  []RetentionRow // per-queue exact bucket counts, gather order
	retentionTrunc bool
}

// buildStorage assembles the report. Pure: no I/O, fully unit-testable.
func buildStorage(in storageInputs) *StorageReport {
	rep := &StorageReport{
		Memory:         make([]QueueMemory, 0, len(in.probes)),
		SamplePerQueue: storageSamplePerQueue,
		Method:         storageMethod,
		Offenders:      make([]PayloadOffender, 0),
		Retention: RetentionHistogram{
			BucketLabels:    retentionBucketLabels,
			Fleet:           RetentionRow{Queue: "", Counts: make([]int64, len(retentionBucketLabels))},
			Queues:          in.retentionRows,
			QueuesTruncated: in.retentionTrunc,
		},
	}
	if rep.Retention.Queues == nil {
		rep.Retention.Queues = make([]RetentionRow, 0)
	}

	sampled := make(map[string]bool, len(in.probes))
	for _, p := range in.probes {
		sampled[p.Queue] = true
		row := QueueMemory{
			Queue:       p.Queue,
			Tasks:       p.Tasks,
			StructBytes: p.StructBytes,
			SampledAt:   p.SampledAt,
		}
		var memSum int64
		var memN int64
		for _, s := range p.Samples {
			if s.MemBytes > 0 {
				memSum += s.MemBytes
				memN++
			}
			if s.MsgBytes > 0 {
				rep.Offenders = append(rep.Offenders, PayloadOffender{
					Queue: p.Queue, ID: s.ID, State: s.State, MsgBytes: s.MsgBytes,
				})
			}
		}
		row.SampledTasks = int(memN)
		if memN > 0 {
			row.AvgTaskBytes = memSum / memN
		}
		row.EstimatedBytes = row.StructBytes + row.AvgTaskBytes*row.Tasks
		rep.Memory = append(rep.Memory, row)
	}
	sort.Slice(rep.Memory, func(i, j int) bool {
		if rep.Memory[i].EstimatedBytes != rep.Memory[j].EstimatedBytes {
			return rep.Memory[i].EstimatedBytes > rep.Memory[j].EstimatedBytes
		}
		return rep.Memory[i].Queue < rep.Memory[j].Queue
	})

	// Offenders: largest first, capped, deterministic.
	sort.Slice(rep.Offenders, func(i, j int) bool {
		if rep.Offenders[i].MsgBytes != rep.Offenders[j].MsgBytes {
			return rep.Offenders[i].MsgBytes > rep.Offenders[j].MsgBytes
		}
		if rep.Offenders[i].Queue != rep.Offenders[j].Queue {
			return rep.Offenders[i].Queue < rep.Offenders[j].Queue
		}
		return rep.Offenders[i].ID < rep.Offenders[j].ID
	})
	if len(rep.Offenders) > maxPayloadOffenders {
		rep.Offenders = rep.Offenders[:maxPayloadOffenders]
	}

	// Not-sampled accounting: queues with tasks that got no probe.
	for _, s := range in.snaps {
		if snapshotSize(s) > 0 && !sampled[s.Queue] {
			rep.NotSampledQueues++
		}
	}

	// Fleet retention = sum of the per-queue rows.
	for _, r := range in.retentionRows {
		for i, c := range r.Counts {
			if i < len(rep.Retention.Fleet.Counts) {
				rep.Retention.Fleet.Counts[i] += c
			}
		}
		rep.Retention.Fleet.Total += r.Total
	}
	return rep
}

func storageSummary(rep *StorageReport) map[string]int64 {
	var est int64
	for _, m := range rep.Memory {
		est += m.EstimatedBytes
	}
	var reclaim24h int64
	// past_due + <1h + 1-6h + 6-24h.
	for i := 0; i < 4 && i < len(rep.Retention.Fleet.Counts); i++ {
		reclaim24h += rep.Retention.Fleet.Counts[i]
	}
	pastDue := int64(0)
	if len(rep.Retention.Fleet.Counts) > 0 {
		pastDue = rep.Retention.Fleet.Counts[0]
	}
	return map[string]int64{
		"sampled_queues":     int64(len(rep.Memory)),
		"not_sampled_queues": int64(rep.NotSampledQueues),
		"estimated_bytes":    est,
		"completed_total":    rep.Retention.Fleet.Total,
		"reclaim_24h":        reclaim24h,
		"reclaim_past_due":   pastDue,
	}
}

// ----------------------------------------------------------------------------
// Gather (bounded Redis reads).
// ----------------------------------------------------------------------------

// stateSizes returns the per-state (name, size, isList) triple for sampling
// decisions, in a fixed order.
func stateSizes(s *stats.QueueSnapshot) []struct {
	name   string
	size   int64
	isList bool
} {
	return []struct {
		name   string
		size   int64
		isList bool
	}{
		{"pending", s.Pending, true},
		{"active", s.Active, true},
		{"scheduled", s.Scheduled, false},
		{"retry", s.Retry, false},
		{"archived", s.Archived, false},
		{"completed", s.Completed, false},
	}
}

func stateKeyFor(q, state string) string {
	switch state {
	case "pending":
		return pendingKey(q)
	case "active":
		return activeKey(q)
	case "scheduled":
		return scheduledKey(q)
	case "retry":
		return retryKey(q)
	case "archived":
		return archivedKey(q)
	default:
		return completedKey(q)
	}
}

// gatherStorage produces the report from live substrates.
func gatherStorage(ctx context.Context, rc redis.UniversalClient, snaps []*stats.QueueSnapshot, now time.Time) (*StorageReport, error) {
	// Top-K queues by task count for memory sampling.
	withTasks := make([]*stats.QueueSnapshot, 0, len(snaps))
	for _, s := range snaps {
		if snapshotSize(s) > 0 {
			withTasks = append(withTasks, s)
		}
	}
	sort.Slice(withTasks, func(i, j int) bool {
		si, sj := snapshotSize(withTasks[i]), snapshotSize(withTasks[j])
		if si != sj {
			return si > sj
		}
		return withTasks[i].Queue < withTasks[j].Queue
	})
	targets := withTasks
	if len(targets) > storageSampleQueues {
		targets = targets[:storageSampleQueues]
	}

	probes := make([]storageProbe, 0, len(targets))
	for _, s := range targets {
		probe, err := gatherQueueProbe(ctx, rc, s, now)
		if err != nil {
			return nil, err
		}
		probes = append(probes, probe)
	}

	retRows, retTrunc, err := gatherRetention(ctx, rc, snaps, now)
	if err != nil {
		return nil, err
	}
	return buildStorage(storageInputs{
		snaps:          snaps,
		probes:         probes,
		retentionRows:  retRows,
		retentionTrunc: retTrunc,
	}), nil
}

// gatherQueueProbe samples one queue: structure MEMORY USAGE + head/tail task
// probes of its two largest states.
func gatherQueueProbe(ctx context.Context, rc redis.UniversalClient, s *stats.QueueSnapshot, now time.Time) (storageProbe, error) {
	probe := storageProbe{Queue: s.Queue, Tasks: snapshotSize(s), SampledAt: now}

	states := stateSizes(s)
	pipe := rc.Pipeline()
	memCmds := make([]*redis.IntCmd, len(states))
	for i, st := range states {
		memCmds[i] = pipe.MemoryUsage(ctx, stateKeyFor(s.Queue, st.name))
	}

	// Sample IDs from the two largest states (head + tail bands — the
	// pending-wait-sample precedent; honest "sampled" label on the report).
	ranked := make([]int, len(states))
	for i := range ranked {
		ranked[i] = i
	}
	sort.Slice(ranked, func(a, b int) bool { return states[ranked[a]].size > states[ranked[b]].size })

	type idCmd struct {
		state string
		cmd   *redis.StringSliceCmd
	}
	var idCmds []idCmd
	per := int64(storageSamplePerQueue / 4) // head + tail of two states
	for _, ri := range ranked[:2] {
		st := states[ri]
		if st.size == 0 {
			continue
		}
		key := stateKeyFor(s.Queue, st.name)
		if st.isList {
			idCmds = append(idCmds,
				idCmd{st.name, pipe.LRange(ctx, key, 0, per-1)},
				idCmd{st.name, pipe.LRange(ctx, key, -per, -1)},
			)
		} else {
			idCmds = append(idCmds,
				idCmd{st.name, pipe.ZRange(ctx, key, 0, per-1)},
				idCmd{st.name, pipe.ZRange(ctx, key, -per, -1)},
			)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return probe, err
	}
	for _, c := range memCmds {
		// MEMORY USAGE on a missing key answers nil — that state is empty.
		if n, err := c.Result(); err == nil {
			probe.StructBytes += n
		}
	}

	// Dedup IDs (head/tail overlap on short structures), keep state labels.
	seen := make(map[string]bool, storageSamplePerQueue)
	type idState struct{ id, state string }
	var ids []idState
	for _, c := range idCmds {
		members, err := c.cmd.Result()
		if err != nil {
			continue
		}
		for _, id := range members {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, idState{id, c.state})
			}
		}
	}
	if len(ids) > storageSamplePerQueue {
		ids = ids[:storageSamplePerQueue]
	}
	if len(ids) == 0 {
		return probe, nil
	}

	pipe2 := rc.Pipeline()
	memC := make([]*redis.IntCmd, len(ids))
	lenC := make([]*redis.Cmd, len(ids))
	for i, is := range ids {
		memC[i] = pipe2.MemoryUsage(ctx, taskKey(s.Queue, is.id))
		// go-redis v9.0.4 has no HStrLen wrapper; HSTRLEN via the generic Do.
		lenC[i] = pipe2.Do(ctx, "hstrlen", taskKey(s.Queue, is.id), "msg")
	}
	if _, err := pipe2.Exec(ctx); err != nil && err != redis.Nil {
		return probe, err
	}
	for i, is := range ids {
		smp := taskSample{ID: is.id, State: is.state}
		// A miss means the task moved/expired between reads — degrade to an
		// unsampled slot, never fabricate.
		if n, err := memC[i].Result(); err == nil {
			smp.MemBytes = n
		}
		if n, err := lenC[i].Int64(); err == nil {
			smp.MsgBytes = n
		}
		if smp.MemBytes > 0 || smp.MsgBytes > 0 {
			probe.Samples = append(probe.Samples, smp)
		}
	}
	return probe, nil
}

// gatherRetention issues the exact per-queue expiry-bucket ZCOUNTs for the
// top retentionQueueCap queues by completed count.
func gatherRetention(ctx context.Context, rc redis.UniversalClient, snaps []*stats.QueueSnapshot, now time.Time) ([]RetentionRow, bool, error) {
	withCompleted := make([]*stats.QueueSnapshot, 0, len(snaps))
	for _, s := range snaps {
		if s.Completed > 0 {
			withCompleted = append(withCompleted, s)
		}
	}
	sort.Slice(withCompleted, func(i, j int) bool {
		if withCompleted[i].Completed != withCompleted[j].Completed {
			return withCompleted[i].Completed > withCompleted[j].Completed
		}
		return withCompleted[i].Queue < withCompleted[j].Queue
	})
	trunc := false
	if len(withCompleted) > retentionQueueCap {
		withCompleted = withCompleted[:retentionQueueCap]
		trunc = true
	}
	if len(withCompleted) == 0 {
		return nil, false, nil
	}

	ranges := retentionBucketRanges(now)
	pipe := rc.Pipeline()
	cmds := make([][]*redis.IntCmd, len(withCompleted))
	for i, s := range withCompleted {
		cmds[i] = make([]*redis.IntCmd, len(ranges))
		for b, r := range ranges {
			cmds[i][b] = pipe.ZCount(ctx, completedKey(s.Queue), r[0], r[1])
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, trunc, err
	}
	rows := make([]RetentionRow, 0, len(withCompleted))
	for i, s := range withCompleted {
		row := RetentionRow{Queue: s.Queue, Counts: make([]int64, len(ranges))}
		for b := range ranges {
			n, _ := cmds[i][b].Result()
			row.Counts[b] = n
			row.Total += n
		}
		rows = append(rows, row)
	}
	return rows, trunc, nil
}
