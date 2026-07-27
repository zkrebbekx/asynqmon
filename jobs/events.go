package jobs

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines the live job-progress event feed:
//   - EventsChannel: the Redis pub/sub channel every Store mutation publishes
//     the affected job's public JSON onto. The replica performing a write is
//     the publisher (multi-replica correct: the claiming runner publishes its
//     progress ticks, whichever replica serves a control-plane POST publishes
//     that transition); every replica's SSE broker subscribes once and fans
//     out to its own clients.
//   - WireJob: the job's public JSON — the SAME frozen frontend contract the
//     /api/jobs endpoints serve (asynqmon's jobJSON is an alias of this type,
//     so the pub/sub payload and the REST body can never drift).
//   - eventPublisher: the per-Store publish throttle. State transitions are
//     always published immediately; progress-only batch writes are coalesced
//     to at most one publish per job per second (batch writes can be far more
//     frequent than that).
//
// Publishing is best-effort by design: a lost PUBLISH costs nothing but
// latency — the SSE on-connect snapshot and the frontend's poll fallback are
// the correctness paths.
// ****************************************************************************

// EventsChannel is the Redis pub/sub channel carrying one WireJob JSON
// payload per job progress tick / state transition.
const EventsChannel = "asynqmon:events:jobs"

// eventMinInterval is the per-job floor between progress-only publishes.
const eventMinInterval = time.Second

// WireJob is the frozen public JSON shape of a job (frontend contract:
// ui/src/api.ts JobInfo). Served by the /api/jobs endpoints and published on
// EventsChannel.
type WireJob struct {
	ID    string `json:"id"`
	Verb  string `json:"verb"`
	Scope Scope  `json:"scope"`
	Phase string `json:"phase"`
	State string `json:"state"`

	Throttle int    `json:"throttle"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`

	Counts      Counts `json:"counts"`
	CostClass   string `json:"cost_class"`
	CostListLen int64  `json:"cost_list_len"`

	PreviewComplete  bool `json:"preview_complete"`
	ProceedOnPartial bool `json:"proceed_on_partial"`

	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	// PreviewCompletedAt is when enumeration finished ("" until then) —
	// additive field, the scan-results banner's "as of" stamp.
	PreviewCompletedAt string `json:"preview_completed_at"`

	Fence            int64  `json:"fence"`
	Error            string `json:"error"`
	FailuresOverflow int64  `json:"failures_overflow"`
	CtlPending       string `json:"ctl_pending"` // "pause"/"cancel" while a request is unhonored
}

// fmtRFC3339 renders wire timestamps ("" for the zero time — absent data is
// not "now").
func fmtRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// WireOf renders one job into its public JSON shape.
func WireOf(j *Job) WireJob {
	scope := j.Scope
	if scope.Meta == nil {
		scope.Meta = []string{}
	}
	return WireJob{
		ID:                 j.ID,
		Verb:               string(j.Verb),
		Scope:              scope,
		Phase:              string(j.Phase),
		State:              string(j.State),
		Throttle:           j.Throttle,
		Reason:             j.Reason,
		Actor:              j.Actor,
		Counts:             j.Counts,
		CostClass:          string(j.CostClass),
		CostListLen:        j.CostListLen,
		PreviewComplete:    j.PreviewComplete,
		ProceedOnPartial:   j.ProceedOnPartial,
		CreatedAt:          fmtRFC3339(j.CreatedAt),
		StartedAt:          fmtRFC3339(j.StartedAt),
		FinishedAt:         fmtRFC3339(j.FinishedAt),
		PreviewCompletedAt: fmtRFC3339(j.PreviewCompletedAt),
		Fence:              j.Fence,
		Error:              j.Error,
		FailuresOverflow:   j.FailuresOverflow,
		CtlPending:         j.Ctl,
	}
}

// eventPublisher throttles and performs the PUBLISHes for one Store. The
// throttle state is in-memory and per-instance on purpose: only the replica
// doing the writing throttles its own ticks, and the only high-frequency
// writer is the claiming runner's batch loop.
type eventPublisher struct {
	rc redis.UniversalClient

	mu   sync.Mutex
	last map[string]time.Time // job id -> last publish time
}

func newEventPublisher(rc redis.UniversalClient) *eventPublisher {
	return &eventPublisher{rc: rc, last: make(map[string]time.Time)}
}

// shouldPublish consumes one publish slot for the job. Forced publishes
// (state transitions) always pass; progress-only publishes pass at most once
// per eventMinInterval per job.
func (p *eventPublisher) shouldPublish(id string, force bool) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !force {
		if t, ok := p.last[id]; ok && now.Sub(t) < eventMinInterval {
			return false
		}
	}
	p.last[id] = now
	// Bound the stamp map: prune stale entries once it grows past any
	// plausible set of concurrently-active jobs.
	if len(p.last) > 512 {
		for k, t := range p.last {
			if now.Sub(t) > 5*time.Minute {
				delete(p.last, k)
			}
		}
	}
	return true
}

// publish renders and PUBLISHes the job's public JSON. Best-effort: errors
// are dropped (the poll fallback and on-connect snapshot cover losses).
// Terminal jobs release their throttle stamp.
func (p *eventPublisher) publish(ctx context.Context, j *Job) {
	if j == nil {
		return
	}
	data, err := json.Marshal(WireOf(j))
	if err != nil {
		return
	}
	_ = p.rc.Publish(ctx, EventsChannel, string(data)).Err()
	if j.State.IsTerminal() {
		p.mu.Lock()
		delete(p.last, j.ID)
		p.mu.Unlock()
	}
}

// publishJob is the Store's one-liner for state transitions (always forced).
func (s *Store) publishJob(ctx context.Context, j *Job) {
	if j == nil {
		return
	}
	s.events.shouldPublish(j.ID, true)
	s.events.publish(ctx, j)
}

// publishProgress re-reads and publishes the job after a successful progress
// write, honoring the per-job throttle. force marks writes that carry a state
// transition (their publish must never be coalesced away).
func (s *Store) publishProgress(ctx context.Context, id string, force bool) {
	if !s.events.shouldPublish(id, force) {
		return
	}
	j, err := s.Get(ctx, id)
	if err != nil {
		return
	}
	s.events.publish(ctx, j)
}
