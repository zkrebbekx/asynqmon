package asynqmon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/aql"
)

// ****************************************************************************
// This file defines the AQL plan executors backing GET /api/tasks?q=<AQL>
// (build contract §3.4, §5.3, §5.9, phase 6):
//   - cursor listing: exact ZRANGEBYSCORE (score,id) pagination + ZCOUNT
//     totals for score-cursorable plans (next_run, past_due, died, expires)
//   - progressive scan: budgeted, resumable enumeration with env preparation
//     for the live-data predicates (workers, orphans, pending_since, group
//     scores)
//   - the pipelined per-state candidate estimator shared with
//     /api/tasks/state_counts
// ****************************************************************************

// asynq's internal Redis key formats, mirrored locally (same version-pinned
// rationale as stats/keys.go — pinned to asynq v0.24.1; readers treat a miss
// as "unknown" and degrade instead of failing). The executors below only
// ever READ these keys; every mutation still goes through the Inspector.
func aqlQueueKeyPrefix(qname string) string { return fmt.Sprintf("asynq:{%s}:", qname) }

func aqlTaskKey(qname, id string) string  { return aqlQueueKeyPrefix(qname) + "t:" + id }
func aqlLeaseKey(qname string) string     { return aqlQueueKeyPrefix(qname) + "lease" }
func aqlGroupKey(qname, g string) string  { return aqlQueueKeyPrefix(qname) + "g:" + g }
func aqlPendingKey(qname string) string   { return aqlQueueKeyPrefix(qname) + "pending" }
func aqlActiveKey(qname string) string    { return aqlQueueKeyPrefix(qname) + "active" }
func aqlScheduledKey(qname string) string { return aqlQueueKeyPrefix(qname) + "scheduled" }
func aqlRetryKey(qname string) string     { return aqlQueueKeyPrefix(qname) + "retry" }
func aqlArchivedKey(qname string) string  { return aqlQueueKeyPrefix(qname) + "archived" }
func aqlCompletedKey(qname string) string { return aqlQueueKeyPrefix(qname) + "completed" }

// aqlZsetKey maps a cursorable state to its zset key.
func aqlZsetKey(state, qname string) string {
	switch state {
	case "scheduled":
		return aqlScheduledKey(qname)
	case "retry":
		return aqlRetryKey(qname)
	case "archived":
		return aqlArchivedKey(qname)
	case "completed":
		return aqlCompletedKey(qname)
	}
	return ""
}

// orphanGrace is the §3.4 orphan definition: lease expired beyond one lease
// interval (asynq workers hold 30s leases).
const orphanGrace = 30 * time.Second

// ----------------------------------------------------------------------------
// Env preparation
// ----------------------------------------------------------------------------

// prepareRequestEnv builds the request-scoped env data a plan needs: worker
// observations (one Servers() call) and per-queue orphan sets (one pipelined
// ZRANGEBYSCORE per queue). Batch-scoped data (pending_since, group scores)
// is filled during the scan by fillBatchEnv.
func prepareRequestEnv(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, plan *aql.Plan, queues []string, now time.Time) (*aql.Env, error) {
	env := &aql.Env{Now: now}
	if plan.NeedsWorkers {
		env.Workers = make(map[string]aql.WorkerObs)
		servers, err := insp.Servers()
		if err != nil {
			return nil, err
		}
		for _, srv := range servers {
			for _, w := range srv.ActiveWorkers {
				env.Workers[aql.EnvKey(w.Queue, w.TaskID)] = aql.WorkerObs{Started: w.Started, Deadline: w.Deadline}
			}
		}
	}
	if plan.NeedsOrphans {
		env.Orphans = make(map[string]bool)
		cutoff := strconv.FormatInt(now.Add(-orphanGrace).Unix(), 10)
		pipe := rc.Pipeline()
		cmds := make([]*redis.StringSliceCmd, len(queues))
		for i, q := range queues {
			cmds[i] = pipe.ZRangeByScore(ctx, aqlLeaseKey(q), &redis.ZRangeBy{Min: "-inf", Max: cutoff})
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		for i, cmd := range cmds {
			ids, _ := cmd.Result()
			for _, id := range ids {
				env.Orphans[aql.EnvKey(queues[i], id)] = true
			}
		}
	}
	if plan.NeedsPendingSince {
		env.PendingSince = make(map[string]time.Time)
	}
	if plan.NeedsGroupScores {
		env.GroupEnteredAt = make(map[string]time.Time)
	}
	return env, nil
}

// fillBatchEnv fills per-batch env data for the tasks about to be matched:
// pending_since (pipelined HGET on the task hashes) and group entry times
// (one ZMSCORE per batch).
func fillBatchEnv(ctx context.Context, rc redis.UniversalClient, plan *aql.Plan, env *aql.Env, qname, gname string, batch []*asynq.TaskInfo) error {
	if plan.NeedsPendingSince && len(batch) > 0 {
		pipe := rc.Pipeline()
		cmds := make([]*redis.StringCmd, len(batch))
		for i, ti := range batch {
			cmds[i] = pipe.HGet(ctx, aqlTaskKey(ti.Queue, ti.ID), "pending_since")
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		for i, cmd := range cmds {
			v, err := cmd.Result()
			if err != nil {
				continue // vanished mid-scan: predicate reports false, honestly
			}
			ns, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil || ns == 0 {
				continue
			}
			env.PendingSince[aql.EnvKey(batch[i].Queue, batch[i].ID)] = time.Unix(0, ns)
		}
	}
	if plan.NeedsGroupScores && gname != "" && len(batch) > 0 {
		members := make([]string, len(batch))
		for i, ti := range batch {
			members[i] = ti.ID
		}
		scores, err := rc.ZMScore(ctx, aqlGroupKey(qname, gname), members...).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		for i, s := range scores {
			if i >= len(batch) || s == 0 {
				continue
			}
			env.GroupEnteredAt[aql.EnvKey(batch[i].Queue, batch[i].ID)] = time.Unix(int64(s), 0)
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// Cursor listing (ModeCursor): exact totals + (score,id) pagination
// ----------------------------------------------------------------------------

func fmtScore(f float64) string {
	switch {
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsInf(f, 1):
		return "+inf"
	default:
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
}

// cursorTotals sums ZCOUNT over the queues for the plan's score range —
// exact at snapshot time.
func cursorTotals(ctx context.Context, rc redis.UniversalClient, plan *aql.Plan, queues []string) (int64, error) {
	pipe := rc.Pipeline()
	cmds := make([]*redis.IntCmd, len(queues))
	for i, q := range queues {
		cmds[i] = pipe.ZCount(ctx, aqlZsetKey(plan.State, q), fmtScore(plan.ScoreMin), fmtScore(plan.ScoreMax))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	var total int64
	for _, c := range cmds {
		n, _ := c.Result()
		total += n
	}
	return total, nil
}

// execCursorPlan lists one page of a cursorable plan: queues in sorted
// order, each traversed by ascending (score, id) via ZRANGEBYSCORE … LIMIT.
// The cursor is the last returned (queue, score, id); resuming fetches from
// that score inclusively and skips already-served ties (zset ties order
// lexically by member, so "member ≤ cursor id at the cursor score" is
// exactly the served set).
func execCursorPlan(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, plan *aql.Plan, queues []string, cursorStr string, size int, pf PayloadFormatter) (tasks []*searchTask, total int64, next string, err error) {
	total, err = cursorTotals(ctx, rc, plan, queues)
	if err != nil {
		return nil, 0, "", err
	}

	var cur aql.Cursor
	haveCur := false
	if cursorStr != "" {
		cur, err = aql.DecodeCursor(cursorStr)
		if err != nil {
			return nil, 0, "", fmt.Errorf("invalid cursor")
		}
		haveCur = true
	}

	tasks = make([]*searchTask, 0, size)
	var last aql.Cursor
	exhausted := true

	for _, qname := range queues {
		if haveCur && qname < cur.Queue {
			continue // already fully served
		}
		key := aqlZsetKey(plan.State, qname)
		minStr := fmtScore(plan.ScoreMin)
		resuming := haveCur && qname == cur.Queue
		if resuming {
			minStr = fmtScore(cur.Score) // inclusive: ties are filtered below
		}
		offset := int64(0)
		for len(tasks) < size {
			want := int64(size-len(tasks)) + 16 // headroom for tie-skips and vanished tasks
			entries, zerr := rc.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
				Min: minStr, Max: fmtScore(plan.ScoreMax), Offset: offset, Count: want,
			}).Result()
			if zerr != nil && !errors.Is(zerr, redis.Nil) {
				return nil, 0, "", zerr
			}
			if len(entries) == 0 {
				break
			}
			for _, e := range entries {
				if len(tasks) >= size {
					exhausted = false
					break
				}
				id, _ := e.Member.(string)
				if resuming && e.Score == cur.Score && id <= cur.ID {
					continue // tie already served on the previous page
				}
				ti, gerr := insp.GetTaskInfo(qname, id)
				if gerr != nil {
					continue // vanished between ZRANGE and fetch: skip, honestly
				}
				if ti.State.String() != plan.State {
					continue // moved mid-listing
				}
				tasks = append(tasks, toSearchTask(ti, pf))
				last = aql.Cursor{Queue: qname, Score: e.Score, ID: id}
			}
			if len(tasks) >= size {
				// More entries may remain in this queue (or later queues).
				exhausted = false
				break
			}
			offset += int64(len(entries))
			if int64(len(entries)) < want {
				break // queue range exhausted
			}
		}
		if len(tasks) >= size {
			break
		}
	}

	if !exhausted && last.Queue != "" {
		next = aql.EncodeCursor(last)
	}
	return tasks, total, next, nil
}

// ----------------------------------------------------------------------------
// Progressive scan (ModeScan): budgeted, resumable (§5.9)
// ----------------------------------------------------------------------------

// scanOutcome is one budgeted scan call's result.
type scanOutcome struct {
	matches []*searchTask
	scanned int    // tasks examined THIS call
	cursor  string // resume cursor; "" when the scan completed
}

// execScanPlan walks the plan's scope in listing pages, applying the
// compiled predicate (plus any extra legacy filters), until the budget is
// spent or the scope is exhausted. The cursor is (queue, group, page) at
// page granularity — the resolved queue order is sorted, so a resume
// continues where the previous call stopped even across requests. Budget is
// TOTAL per call (unlike the legacy per-queue max_scan), because the §5.9
// meter reports one number.
func execScanPlan(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, plan *aql.Plan, env *aql.Env, queues []string, extra func(*searchTask) bool, cursorStr string, budget int, pf PayloadFormatter) (*scanOutcome, error) {
	var cur aql.ScanCursor
	if cursorStr != "" {
		var err error
		cur, err = aql.DecodeScanCursor(cursorStr)
		if err != nil {
			return nil, fmt.Errorf("invalid scan_cursor")
		}
	}

	out := &scanOutcome{matches: make([]*searchTask, 0)}

	for _, qname := range queues {
		if cur.Queue != "" && qname < cur.Queue {
			continue
		}
		resumingQueue := cur.Queue != "" && qname == cur.Queue

		// Group iteration (aggregating only). A plan-scoped group= narrows
		// the walk to that single group.
		groups := []string{""}
		if plan.State == "aggregating" {
			ginfos, gerr := insp.Groups(qname)
			if gerr != nil {
				if errors.Is(gerr, asynq.ErrQueueNotFound) {
					continue
				}
				return nil, gerr
			}
			groups = groups[:0]
			for _, g := range ginfos {
				if plan.Group != "" && g.Group != plan.Group {
					continue
				}
				groups = append(groups, g.Group)
			}
			sort.Strings(groups)
		}

		for _, gname := range groups {
			if resumingQueue && gname < cur.Group {
				continue
			}
			page := 1
			if resumingQueue && gname == cur.Group && cur.Page > 0 {
				page = cur.Page
			}
			for {
				if out.scanned >= budget {
					out.cursor = aql.EncodeScanCursor(aql.ScanCursor{Queue: qname, Group: gname, Page: page})
					return out, nil
				}
				batch, lerr := listTasksByState(insp, qname, gname, plan.State, page, searchBatchSize)
				if lerr != nil {
					if errors.Is(lerr, asynq.ErrQueueNotFound) {
						break // queue removed mid-scan; skip it
					}
					return nil, lerr
				}
				if len(batch) == 0 {
					break
				}
				if err := fillBatchEnv(ctx, rc, plan, env, qname, gname, batch); err != nil {
					return nil, err
				}
				for _, ti := range batch {
					if !plan.Match(env, ti) {
						continue
					}
					st := toSearchTask(ti, pf)
					if extra != nil && !extra(st) {
						continue
					}
					out.matches = append(out.matches, st)
				}
				out.scanned += len(batch)
				if len(batch) < searchBatchSize {
					break // this queue/group is exhausted
				}
				page++
			}
		}
	}
	return out, nil // scanned to completion: no cursor
}

// ----------------------------------------------------------------------------
// Candidate estimate + state counts: one pipelined pass
// ----------------------------------------------------------------------------

// stateCountsForQueues reads all seven per-state sizes for the given queues
// in one pipelined pass (LLEN for the LIST states, ZCARD for the zsets),
// plus a second bounded pass for aggregating (group zset cards). Returns
// per-state totals summed across the queues.
func stateCountsForQueues(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, queues []string) (map[string]int64, error) {
	counts := map[string]int64{
		"pending": 0, "active": 0, "scheduled": 0, "retry": 0,
		"archived": 0, "completed": 0, "aggregating": 0,
	}
	pipe := rc.Pipeline()
	type qc struct{ pending, active, scheduled, retry, archived, completed *redis.IntCmd }
	cmds := make([]qc, len(queues))
	for i, q := range queues {
		cmds[i] = qc{
			pending:   pipe.LLen(ctx, aqlPendingKey(q)),
			active:    pipe.LLen(ctx, aqlActiveKey(q)),
			scheduled: pipe.ZCard(ctx, aqlScheduledKey(q)),
			retry:     pipe.ZCard(ctx, aqlRetryKey(q)),
			archived:  pipe.ZCard(ctx, aqlArchivedKey(q)),
			completed: pipe.ZCard(ctx, aqlCompletedKey(q)),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	for _, c := range cmds {
		n, _ := c.pending.Result()
		counts["pending"] += n
		n, _ = c.active.Result()
		counts["active"] += n
		n, _ = c.scheduled.Result()
		counts["scheduled"] += n
		n, _ = c.retry.Result()
		counts["retry"] += n
		n, _ = c.archived.Result()
		counts["archived"] += n
		n, _ = c.completed.Result()
		counts["completed"] += n
	}
	agg, err := aggregatingCount(insp, queues, "")
	if err != nil {
		return nil, err
	}
	counts["aggregating"] = agg
	return counts, nil
}

// aggregatingCount sums group sizes across the queues (optionally one named
// group) via Inspector.Groups — public API, no key mirroring.
func aggregatingCount(insp *asynq.Inspector, queues []string, group string) (int64, error) {
	var total int64
	for _, q := range queues {
		ginfos, err := insp.Groups(q)
		if err != nil {
			if errors.Is(err, asynq.ErrQueueNotFound) {
				continue
			}
			return 0, err
		}
		for _, g := range ginfos {
			if group != "" && g.Group != group {
				continue
			}
			total += int64(g.Size)
		}
	}
	return total, nil
}

// estimateCandidates prices a scan: the total number of tasks in the scanned
// state across the resolved queues (one pipelined size read). It is an
// estimate of scan WORK, not of matches — the §5.9 meter's "~M candidates".
func estimateCandidates(ctx context.Context, rc redis.UniversalClient, insp *asynq.Inspector, state string, queues []string, group string) (int64, error) {
	if state == "aggregating" {
		return aggregatingCount(insp, queues, group)
	}
	pipe := rc.Pipeline()
	cmds := make([]*redis.IntCmd, len(queues))
	for i, q := range queues {
		switch state {
		case "pending":
			cmds[i] = pipe.LLen(ctx, aqlPendingKey(q))
		case "active":
			cmds[i] = pipe.LLen(ctx, aqlActiveKey(q))
		default:
			cmds[i] = pipe.ZCard(ctx, aqlZsetKey(state, q))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	var total int64
	for _, c := range cmds {
		n, _ := c.Result()
		total += n
	}
	return total, nil
}
