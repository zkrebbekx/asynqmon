package hygiene

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/errsig"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Dead-letter digest (§3.10): archived age distribution per queue (exact
// died-at ZCOUNT buckets), top error clusters per queue (errsig index read
// path), trim-cap flags, and a one-click action descriptor per queue —
// "delete archived older than 30d" — that the frontend turns into a
// BulkJobModal open with scope {queue, state=archived, aql "died>30d"}.
//
// Bounds per run: the top deadLetterQueueCap queues by archived count get 4
// pipelined ZCOUNTs each; the errsig index is read once (its own store is
// capped at 1,000 signatures).
// ****************************************************************************

const (
	// deadLetterQueueCap bounds the per-queue rows (top by archived count).
	deadLetterQueueCap = 100
	// clustersPerQueue caps the error clusters attached to each row.
	clustersPerQueue = 3
	// deadLetterThresholdDays is the "older than X" of the one-click action.
	deadLetterThresholdDays = 30
)

// deadLetterBucketLabels is the frozen age-bucket order (age = now − died-at).
var deadLetterBucketLabels = []string{"<1d", "1-7d", "7-30d", ">30d"}

// deadLetterBucketRanges returns the [min,max] ZCOUNT bounds (died-at Unix
// second strings) per age bucket at the given instant. Pure — unit-tested.
// Newer deaths have HIGHER scores, so "<1d" covers (now-1d, +inf).
func deadLetterBucketRanges(now time.Time) [][2]string {
	sec := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	d1 := now.Add(-24 * time.Hour)
	d7 := now.Add(-7 * 24 * time.Hour)
	d30 := now.Add(-30 * 24 * time.Hour)
	return [][2]string{
		{"(" + sec(d1), "+inf"},   // <1d
		{"(" + sec(d7), sec(d1)},  // 1-7d
		{"(" + sec(d30), sec(d7)}, // 7-30d
		{"-inf", sec(d30)},        // >30d
	}
}

// DeadLetterCluster is one error cluster observed in a queue's archive.
type DeadLetterCluster struct {
	Sig      string `json:"sig"`
	Template string `json:"template"`
	// Archived is the errsig index's exact archived-observation count for
	// this (queue, signature); LowerBound flags counts from a queue at the
	// archive trim cap.
	Archived   int64 `json:"archived"`
	LowerBound bool  `json:"lower_bound"`
}

// DeadLetterAction is the one-click job descriptor: the frontend opens
// BulkJobModal with exactly this scope — the job flow (preview, typed
// confirmation, audit) stays the standard §4.3 path.
type DeadLetterAction struct {
	Label string `json:"label"`
	Verb  string `json:"verb"` // "delete"
	Queue string `json:"queue"`
	State string `json:"state"` // "archived"
	Aql   string `json:"aql"`   // "died>30d"
	// Estimate is the exact >threshold bucket count at generation time (the
	// job's own preview recounts live).
	Estimate int64 `json:"estimate"`
}

// DeadLetterQueue is one queue of the digest.
type DeadLetterQueue struct {
	Queue string `json:"queue"`
	// Archived is the sum of the exact age buckets at generation time.
	Archived int64 `json:"archived"`
	// Buckets follow the report's BucketLabels order.
	Buckets            []int64 `json:"buckets"`
	OlderThanThreshold int64   `json:"older_than_threshold"`
	// AtTrimCap: the archive sits at asynq's 10k cap — data is being
	// discarded and every count here is a lower bound.
	AtTrimCap bool                `json:"at_trim_cap"`
	Clusters  []DeadLetterCluster `json:"clusters"`
	Action    *DeadLetterAction   `json:"action,omitempty"`
}

// DeadLetterReport is the dead-letter-digest payload.
type DeadLetterReport struct {
	BucketLabels        []string          `json:"bucket_labels"`
	ActionThresholdDays int               `json:"action_threshold_days"`
	Queues              []DeadLetterQueue `json:"queues"`
	QueuesWithArchived  int               `json:"queues_with_archived"`
	QueuesTruncated     bool              `json:"queues_truncated"`
	// ClustersIndexedAt stamps the errsig tail sweep the clusters came from
	// (zero = index empty/never fed — rows then carry no clusters).
	ClustersIndexedAt time.Time `json:"clusters_indexed_at,omitempty"`
}

// deadLetterQueueBuckets is one queue's gathered exact bucket counts.
type deadLetterQueueBuckets struct {
	Queue   string
	Buckets []int64 // deadLetterBucketLabels order
}

// deadLetterInputs are buildDeadLetter's pure inputs.
type deadLetterInputs struct {
	buckets            []deadLetterQueueBuckets
	queuesWithArchived int
	truncated          bool
	sigs               []*errsig.Signature
	meta               *errsig.Meta // nil tolerated (index never fed)
}

// buildDeadLetter assembles the report. Pure: no I/O, fully unit-testable.
func buildDeadLetter(in deadLetterInputs) *DeadLetterReport {
	rep := &DeadLetterReport{
		BucketLabels:        deadLetterBucketLabels,
		ActionThresholdDays: deadLetterThresholdDays,
		Queues:              make([]DeadLetterQueue, 0, len(in.buckets)),
		QueuesWithArchived:  in.queuesWithArchived,
		QueuesTruncated:     in.truncated,
	}
	trimmed := make(map[string]bool)
	if in.meta != nil {
		rep.ClustersIndexedAt = in.meta.TailAt
		for _, q := range in.meta.TrimmedQueues {
			trimmed[q] = true
		}
	}

	// Per-queue cluster candidates from the errsig matrix: archived counts
	// summed across task types.
	perQueueSig := make(map[string]map[*errsig.Signature]int64)
	for _, s := range in.sigs {
		for _, c := range s.Counts {
			if c.Archived <= 0 {
				continue
			}
			m := perQueueSig[c.Queue]
			if m == nil {
				m = make(map[*errsig.Signature]int64)
				perQueueSig[c.Queue] = m
			}
			m[s] += c.Archived
		}
	}

	for _, qb := range in.buckets {
		var total int64
		for _, n := range qb.Buckets {
			total += n
		}
		row := DeadLetterQueue{
			Queue:     qb.Queue,
			Archived:  total,
			Buckets:   qb.Buckets,
			AtTrimCap: total >= errsig.ArchiveTrimCap,
			Clusters:  make([]DeadLetterCluster, 0, clustersPerQueue),
		}
		if len(qb.Buckets) == len(deadLetterBucketLabels) {
			row.OlderThanThreshold = qb.Buckets[len(qb.Buckets)-1]
		}

		type sigCount struct {
			sig *errsig.Signature
			n   int64
		}
		var candidates []sigCount
		for s, n := range perQueueSig[qb.Queue] {
			candidates = append(candidates, sigCount{s, n})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].n != candidates[j].n {
				return candidates[i].n > candidates[j].n
			}
			return candidates[i].sig.Sig < candidates[j].sig.Sig
		})
		if len(candidates) > clustersPerQueue {
			candidates = candidates[:clustersPerQueue]
		}
		for _, c := range candidates {
			row.Clusters = append(row.Clusters, DeadLetterCluster{
				Sig:        c.sig.Sig,
				Template:   c.sig.Template,
				Archived:   c.n,
				LowerBound: trimmed[qb.Queue] || row.AtTrimCap,
			})
		}

		if row.OlderThanThreshold > 0 {
			row.Action = &DeadLetterAction{
				Label:    fmt.Sprintf("Delete archived older than %dd", deadLetterThresholdDays),
				Verb:     "delete",
				Queue:    qb.Queue,
				State:    "archived",
				Aql:      fmt.Sprintf("died>%dd", deadLetterThresholdDays),
				Estimate: row.OlderThanThreshold,
			}
		}
		rep.Queues = append(rep.Queues, row)
	}

	sort.Slice(rep.Queues, func(i, j int) bool {
		if rep.Queues[i].Archived != rep.Queues[j].Archived {
			return rep.Queues[i].Archived > rep.Queues[j].Archived
		}
		return rep.Queues[i].Queue < rep.Queues[j].Queue
	})
	return rep
}

func deadLetterSummary(rep *DeadLetterReport) map[string]int64 {
	sum := map[string]int64{
		"archived_total":       0,
		"older_than_30d":       0,
		"trim_cap_queues":      0,
		"queues_with_archived": int64(rep.QueuesWithArchived),
	}
	for _, q := range rep.Queues {
		sum["archived_total"] += q.Archived
		sum["older_than_30d"] += q.OlderThanThreshold
		if q.AtTrimCap {
			sum["trim_cap_queues"]++
		}
	}
	return sum
}

// ----------------------------------------------------------------------------
// Gather (bounded Redis reads).
// ----------------------------------------------------------------------------

// gatherDeadLetter produces the report from live substrates.
func gatherDeadLetter(ctx context.Context, rc redis.UniversalClient, snaps []*stats.QueueSnapshot, now time.Time) (*DeadLetterReport, error) {
	withArchived := make([]*stats.QueueSnapshot, 0, len(snaps))
	for _, s := range snaps {
		if s.Archived > 0 {
			withArchived = append(withArchived, s)
		}
	}
	sort.Slice(withArchived, func(i, j int) bool {
		if withArchived[i].Archived != withArchived[j].Archived {
			return withArchived[i].Archived > withArchived[j].Archived
		}
		return withArchived[i].Queue < withArchived[j].Queue
	})
	total := len(withArchived)
	trunc := false
	if len(withArchived) > deadLetterQueueCap {
		withArchived = withArchived[:deadLetterQueueCap]
		trunc = true
	}

	ranges := deadLetterBucketRanges(now)
	buckets := make([]deadLetterQueueBuckets, 0, len(withArchived))
	if len(withArchived) > 0 {
		pipe := rc.Pipeline()
		cmds := make([][]*redis.IntCmd, len(withArchived))
		for i, s := range withArchived {
			cmds[i] = make([]*redis.IntCmd, len(ranges))
			for b, r := range ranges {
				cmds[i][b] = pipe.ZCount(ctx, archivedKey(s.Queue), r[0], r[1])
			}
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return nil, err
		}
		for i, s := range withArchived {
			qb := deadLetterQueueBuckets{Queue: s.Queue, Buckets: make([]int64, len(ranges))}
			for b := range ranges {
				n, _ := cmds[i][b].Result()
				qb.Buckets[b] = n
			}
			buckets = append(buckets, qb)
		}
	}

	sigs, meta, err := errsig.ReadIndex(ctx, rc)
	if err != nil {
		return nil, err
	}
	return buildDeadLetter(deadLetterInputs{
		buckets:            buckets,
		queuesWithArchived: total,
		truncated:          trunc,
		sigs:               sigs,
		meta:               meta,
	}), nil
}
