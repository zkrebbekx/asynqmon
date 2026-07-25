package aql

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the batch-time env builder (build contract phase 12,
// scope "AQL env-at-scale"): the machinery that lets env-dependent predicates
// (pending_age>, running>, deadline_pct>, orphaned, group_age>) execute
// OUTSIDE the search request path — most importantly inside the bulk-job
// runner, which previously rejected them at job-create time because its
// re-verification had TaskInfo only.
//
//   - EnvBuilder: configuration (redis + inspector + windows)
//   - Session:    one execution's env state — window data (Servers(), orphan
//     sets) refreshed at most once per RefreshWindow, batch data
//     (pending_since, group scores) pipelined per batch of tasks
//
// The window semantics are the §5.1 freshness discipline: worker tables and
// lease orphan sets are point-in-time observations that stay valid for a few
// seconds; re-reading them per BATCH would multiply Servers() calls by the
// batch count for zero honesty gain, so a session reads them once per
// RefreshWindow (default 15s — the same bound the candidate estimator uses
// for the stats cache) and stamps Env.Now at every refresh.
//
// The asynq key formats mirrored below follow the same version-pinned
// convention as stats/keys.go (asynq@v0.24.1 internals; a miss degrades to
// "no env entry" → the predicate reports false, honestly).
// ****************************************************************************

// Env-builder defaults.
const (
	// DefaultOrphanGrace mirrors §3.4: lease expired beyond one asynq worker
	// lease interval (30s).
	DefaultOrphanGrace = 30 * time.Second
	// DefaultRefreshWindow bounds how stale a session's window data
	// (Servers(), orphan sets) may get before the next Prepare refreshes it.
	DefaultRefreshWindow = 15 * time.Second
)

func envQueueKeyPrefix(qname string) string { return fmt.Sprintf("asynq:{%s}:", qname) }
func envTaskKey(qname, id string) string    { return envQueueKeyPrefix(qname) + "t:" + id }
func envLeaseKey(qname string) string       { return envQueueKeyPrefix(qname) + "lease" }
func envGroupKey(qname, g string) string    { return envQueueKeyPrefix(qname) + "g:" + g }

// EnvBuilder builds per-execution env sessions. Zero-value fields default
// sensibly; RC and Inspector are required for sessions whose plan needs env
// data (a plan that needs none never touches either).
type EnvBuilder struct {
	RC          redis.UniversalClient
	Inspector   *asynq.Inspector
	OrphanGrace time.Duration // default DefaultOrphanGrace
	// RefreshWindow bounds window-data staleness. Default DefaultRefreshWindow.
	RefreshWindow time.Duration
	// Now supplies the clock (injected by tests). Default time.Now.
	Now func() time.Time
}

func (b *EnvBuilder) orphanGrace() time.Duration {
	if b.OrphanGrace > 0 {
		return b.OrphanGrace
	}
	return DefaultOrphanGrace
}

func (b *EnvBuilder) refreshWindow() time.Duration {
	if b.RefreshWindow > 0 {
		return b.RefreshWindow
	}
	return DefaultRefreshWindow
}

func (b *EnvBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Session is one execution's env state for a compiled plan. Not safe for
// concurrent use (the job runner works one job per goroutine).
type Session struct {
	b    *EnvBuilder
	plan *Plan
	env  *Env

	// windowAt is when window data (Workers/Orphans) was last built.
	windowAt time.Time
	// orphanFetched tracks which queues' orphan sets the current window has.
	orphanFetched map[string]bool
}

// NewSession builds a session for a compiled plan. Returns an error when the
// plan needs env data but the builder lacks its dependencies — callers that
// cannot supply them should keep using CompileMatcher (env-free, structured
// rejection).
func (b *EnvBuilder) NewSession(plan *Plan) (*Session, error) {
	needs := plan.NeedsWorkers || plan.NeedsOrphans || plan.NeedsPendingSince || plan.NeedsGroupScores
	if needs && (b == nil || b.RC == nil || b.Inspector == nil) {
		return nil, errors.New("aql: plan needs live env data but the env builder has no redis client / inspector")
	}
	return &Session{b: b, plan: plan, env: &Env{Now: b.now()}, orphanFetched: map[string]bool{}}, nil
}

// Match evaluates the plan's full predicate against the session's current
// env. Call Prepare with the task's batch first; tasks never prepared report
// false on env predicates — honesty over guessing.
func (s *Session) Match(ti *asynq.TaskInfo) bool {
	return s.plan.Match(s.env, ti)
}

// Prepare readies the env for one batch of tasks: refreshes window data
// (Servers(), orphan sets — once per RefreshWindow) and pipelines batch data
// (pending_since, group scores) for exactly the tasks given.
func (s *Session) Prepare(ctx context.Context, batch []*asynq.TaskInfo) error {
	plan := s.plan
	if !plan.NeedsWorkers && !plan.NeedsOrphans && !plan.NeedsPendingSince && !plan.NeedsGroupScores {
		return nil
	}
	now := s.b.now()

	// Window refresh: Servers() and orphan sets at most once per window.
	if s.windowAt.IsZero() || now.Sub(s.windowAt) > s.b.refreshWindow() {
		s.windowAt = now
		s.env.Now = now
		if plan.NeedsWorkers {
			s.env.Workers = make(map[string]WorkerObs)
			servers, err := s.b.Inspector.Servers()
			if err != nil {
				return fmt.Errorf("reading servers for env: %w", err)
			}
			for _, srv := range servers {
				for _, w := range srv.ActiveWorkers {
					s.env.Workers[EnvKey(w.Queue, w.TaskID)] = WorkerObs{Started: w.Started, Deadline: w.Deadline}
				}
			}
		}
		if plan.NeedsOrphans {
			s.env.Orphans = make(map[string]bool)
			s.orphanFetched = map[string]bool{}
		}
	}

	// Orphan sets for this batch's queues (cached within the window).
	if plan.NeedsOrphans {
		var missing []string
		seen := map[string]bool{}
		for _, ti := range batch {
			if !s.orphanFetched[ti.Queue] && !seen[ti.Queue] {
				seen[ti.Queue] = true
				missing = append(missing, ti.Queue)
			}
		}
		if len(missing) > 0 {
			cutoff := strconv.FormatInt(s.env.Now.Add(-s.b.orphanGrace()).Unix(), 10)
			pipe := s.b.RC.Pipeline()
			cmds := make([]*redis.StringSliceCmd, len(missing))
			for i, q := range missing {
				cmds[i] = pipe.ZRangeByScore(ctx, envLeaseKey(q), &redis.ZRangeBy{Min: "-inf", Max: cutoff})
			}
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				return fmt.Errorf("reading orphan sets for env: %w", err)
			}
			for i, cmd := range cmds {
				ids, _ := cmd.Result()
				for _, id := range ids {
					s.env.Orphans[EnvKey(missing[i], id)] = true
				}
				s.orphanFetched[missing[i]] = true
			}
		}
	}

	// pending_since per batch (pipelined HGET on the task hashes).
	if plan.NeedsPendingSince && len(batch) > 0 {
		if s.env.PendingSince == nil {
			s.env.PendingSince = make(map[string]time.Time)
		}
		pipe := s.b.RC.Pipeline()
		cmds := make([]*redis.StringCmd, len(batch))
		for i, ti := range batch {
			cmds[i] = pipe.HGet(ctx, envTaskKey(ti.Queue, ti.ID), "pending_since")
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("reading pending_since for env: %w", err)
		}
		for i, cmd := range cmds {
			v, err := cmd.Result()
			if err != nil {
				continue // dequeued mid-batch: predicate reports false, honestly
			}
			if ns, perr := strconv.ParseInt(v, 10, 64); perr == nil && ns > 0 {
				s.env.PendingSince[EnvKey(batch[i].Queue, batch[i].ID)] = time.Unix(0, ns)
			}
		}
	}

	// Group entry times per batch: one ZMSCORE per (queue, group) present.
	if plan.NeedsGroupScores && len(batch) > 0 {
		if s.env.GroupEnteredAt == nil {
			s.env.GroupEnteredAt = make(map[string]time.Time)
		}
		type qg struct{ q, g string }
		groups := map[qg][]*asynq.TaskInfo{}
		for _, ti := range batch {
			g := ti.Group
			if g == "" {
				g = s.plan.Group
			}
			if g == "" {
				continue
			}
			k := qg{q: ti.Queue, g: g}
			groups[k] = append(groups[k], ti)
		}
		for k, tis := range groups {
			members := make([]string, len(tis))
			for i, ti := range tis {
				members[i] = ti.ID
			}
			scores, err := s.b.RC.ZMScore(ctx, envGroupKey(k.q, k.g), members...).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return fmt.Errorf("reading group scores for env: %w", err)
			}
			for i, sc := range scores {
				if i >= len(tis) || sc == 0 {
					continue
				}
				s.env.GroupEnteredAt[EnvKey(tis[i].Queue, tis[i].ID)] = time.Unix(int64(sc), 0)
			}
		}
	}
	return nil
}
