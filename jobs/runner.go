package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - Runner: the background goroutine pool that claims and works jobs
//     (§5.4 janitor pattern, §5.13 claim-based resume)
//   - the preview enumeration (chunked, cursor-persisted, budgeted)
//   - the execute loop (chunks of 100, throttled, per-task re-fetch +
//     re-match before acting, per-item failure capture)
// ****************************************************************************

const (
	defaultConcurrency  = 2 // max jobs worked at once per replica (§5.4 knob)
	defaultPollInterval = 2 * time.Second
	defaultLeaseTTL     = 15 * time.Second
	defaultPreviewBatch = 1000 // tasks listed per enumeration page
	defaultExecBatch    = 100  // §5.4: act in batches of 100
	defaultPreviewSleep = 25 * time.Millisecond

	// claimScanLimit bounds how many recent jobs each poll inspects for
	// claimable work. Older-than-that unfinished jobs are effectively
	// abandoned; the index cap and TTL age them out.
	claimScanLimit = 200

	// pausedPollEvery is how often a claiming runner re-reads a paused job.
	pausedPollEvery = time.Second
)

// asynq's pending/active LIST keys, mirrored (same version-pinned rationale
// as stats/keys.go) only to price the list_removal cost class with a single
// LLEN per queue. The runner never writes under "asynq:".
func asynqPendingKey(qname string) string { return fmt.Sprintf("asynq:{%s}:pending", qname) }
func asynqActiveKey(qname string) string  { return fmt.Sprintf("asynq:{%s}:active", qname) }

// Config configures a Runner.
type Config struct {
	// RedisClient is the shared connection. Required.
	RedisClient redis.UniversalClient

	// Inspector performs enumeration, re-fetch, and the mutation verbs.
	// Required.
	Inspector *asynq.Inspector

	// Concurrency is the max number of jobs this replica works at once.
	// Default 2.
	Concurrency int

	// PollInterval between claim scans. Default 2s.
	PollInterval time.Duration

	// LeaseTTL of a per-job claim; renewed at ~1/3 TTL. Default 15s.
	LeaseTTL time.Duration

	// PreviewBatch is the tasks-per-page enumeration budget. Default 1000.
	PreviewBatch int

	// ExecBatch is the acting chunk size. Default 100 (janitor pattern).
	ExecBatch int

	// PreviewSleep between enumeration pages, so previews never monopolize
	// Redis. Default 25ms.
	PreviewSleep time.Duration

	// InstanceID identifies this replica in claims. Default "hostname:pid:<rand>".
	InstanceID string

	// Logf logs runner lifecycle events. Default log.Printf.
	Logf func(format string, args ...interface{})
}

// Runner claims jobs from the shared store and works them. Any replica may
// accept a job POST; the runner that wins the per-job claim executes it, and
// a crashed replica's jobs are resumed by the next claimer from the
// persisted cursor (§5.13).
type Runner struct {
	cfg   Config
	store *Store
	insp  *asynq.Inspector
	logf  func(format string, args ...interface{})

	working sync.Map // job id -> struct{} (jobs this replica is working)
	slots   chan struct{}

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewRunner creates a Runner. It does not touch Redis until Start.
func NewRunner(cfg Config) *Runner {
	if cfg.RedisClient == nil {
		panic("jobs.NewRunner: RedisClient is required")
	}
	if cfg.Inspector == nil {
		panic("jobs.NewRunner: Inspector is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.PreviewBatch <= 0 {
		cfg.PreviewBatch = defaultPreviewBatch
	}
	if cfg.ExecBatch <= 0 {
		cfg.ExecBatch = defaultExecBatch
	}
	if cfg.PreviewSleep <= 0 {
		cfg.PreviewSleep = defaultPreviewSleep
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Runner{
		cfg:   cfg,
		store: NewStore(cfg.RedisClient),
		insp:  cfg.Inspector,
		logf:  logf,
		slots: make(chan struct{}, cfg.Concurrency),
	}
}

func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Store exposes the runner's store (shared persistence layer).
func (r *Runner) Store() *Store { return r.store }

// InstanceID returns this replica's claim identity.
func (r *Runner) InstanceID() string { return r.cfg.InstanceID }

// Start launches the claim loop. Non-blocking; a second Start before Stop is
// a no-op (same lifecycle contract as stats.Engine).
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	ctx, r.cancel = context.WithCancel(ctx)
	r.wg.Add(1)
	go r.claimLoop(ctx)
}

// Stop cancels the claim loop and all in-flight job work, then waits for
// everything to exit. In-flight jobs stay claim-resumable: their cursor was
// persisted at the last batch boundary and the claim lease expires by TTL.
func (r *Runner) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	r.started = false
	cancel := r.cancel
	r.mu.Unlock()

	cancel()
	r.wg.Wait()
}

// claimLoop periodically scans recent jobs for claimable work.
func (r *Runner) claimLoop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		r.claimTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// claimTick claims as many workable jobs as free slots allow.
func (r *Runner) claimTick(ctx context.Context) {
	jobsList, err := r.store.List(ctx, claimScanLimit)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("asynqmon: jobs: listing claimable jobs: %v", err)
		}
		return
	}
	// Work oldest-first so a backlog drains in submission order.
	sort.Slice(jobsList, func(i, j int) bool { return jobsList[i].CreatedAt.Before(jobsList[j].CreatedAt) })
	for _, j := range jobsList {
		if !claimableState(j) {
			continue
		}
		if _, ours := r.working.Load(j.ID); ours {
			continue
		}
		select {
		case r.slots <- struct{}{}:
		default:
			return // all slots busy
		}
		token, err := r.store.Claim(ctx, j.ID, r.cfg.InstanceID, r.cfg.LeaseTTL)
		if err != nil || token == 0 {
			<-r.slots
			if err != nil && ctx.Err() == nil {
				r.logf("asynqmon: jobs: claiming %s: %v", j.ID, err)
			}
			continue // held by another replica
		}
		r.working.Store(j.ID, struct{}{})
		r.wg.Add(1)
		go func(id string, token int64) {
			defer r.wg.Done()
			defer func() { r.working.Delete(id); <-r.slots }()
			r.work(ctx, id, token)
		}(j.ID, token)
	}
}

// claimableState: previewing and running jobs need a worker; paused jobs
// need a watcher so a resume is picked up even after the pausing replica
// died. preview_ready jobs are dormant until an execute request flips them
// to running.
func claimableState(j *Job) bool {
	return j.State == StatePreviewing || j.State == StateRunning || j.State == StatePaused
}

// ----------------------------------------------------------------------------
// Working one claimed job
// ----------------------------------------------------------------------------

// claimSession tracks one claim's lease renewal; lost() flips when renewal
// fails so the work loop can stop early instead of discovering the lost
// fence at the next guarded write (the write guard remains the hard gate).
type claimSession struct {
	lost int32
	stop chan struct{}
}

func (r *Runner) startRenewal(ctx context.Context, id string) *claimSession {
	cs := &claimSession{stop: make(chan struct{})}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.cfg.LeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-cs.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := r.store.RenewClaim(ctx, id, r.cfg.InstanceID, r.cfg.LeaseTTL)
				if err != nil || !ok {
					if err != nil && ctx.Err() == nil {
						r.logf("asynqmon: jobs: renewing claim on %s: %v", id, err)
					}
					atomic.StoreInt32(&cs.lost, 1)
					return
				}
			}
		}
	}()
	return cs
}

func (cs *claimSession) isLost() bool { return atomic.LoadInt32(&cs.lost) == 1 }

// work drives one claimed job to a parking or terminal state.
func (r *Runner) work(ctx context.Context, id string, token int64) {
	cs := r.startRenewal(ctx, id)
	defer close(cs.stop)
	defer func() {
		// Best-effort release so another replica (or an execute request) is
		// picked up without waiting out the TTL.
		relCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.store.ReleaseClaim(relCtx, id, r.cfg.InstanceID)
	}()

	j, err := r.store.Get(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			r.logf("asynqmon: jobs: loading claimed job %s: %v", id, err)
		}
		return
	}

	if !j.PreviewComplete {
		done := r.enumerate(ctx, j, token, cs)
		if !done {
			return // canceled, paused-and-lost, fence lost, ctx done, or error — all logged/persisted
		}
		// Re-read: CompletePreview may have parked it (preview phase) or an
		// execute request may already have flipped it to running.
		j, err = r.store.Get(ctx, id)
		if err != nil {
			return
		}
	}

	if j.Phase == PhaseExecute && j.State == StateRunning {
		r.execute(ctx, j, token, cs)
		return
	}
	if j.State == StatePaused {
		// Claimed a paused job: hold the claim and watch for resume/cancel.
		if next := r.waitWhilePaused(ctx, id, token, cs); next != nil && next.Phase == PhaseExecute && next.State == StateRunning {
			r.execute(ctx, next, token, cs)
		}
	}
	// preview_ready: park (release claim via defer) until execute.
}

// checkCtl handles a pending pause/cancel request between batches. Returns
// the (possibly re-read) job and whether work may continue.
func (r *Runner) checkCtl(ctx context.Context, j *Job, token int64, cs *claimSession) (*Job, bool) {
	fresh, err := r.store.Get(ctx, j.ID)
	if err != nil {
		return j, false
	}
	switch fresh.Ctl {
	case CtlCancel:
		r.finalize(ctx, fresh, token, StateCanceled, "")
		return fresh, false
	case CtlPause:
		if ok, _ := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{
			Fields: map[string]string{"state": string(StatePaused), "ctl": ""},
		}); !ok {
			return fresh, false
		}
		next := r.waitWhilePaused(ctx, j.ID, token, cs)
		if next == nil {
			return fresh, false
		}
		return next, true
	}
	return fresh, true
}

// waitWhilePaused polls a paused job until it is resumed (returns the fresh
// job) or canceled / lost (returns nil).
func (r *Runner) waitWhilePaused(ctx context.Context, id string, token int64, cs *claimSession) *Job {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pausedPollEvery):
		}
		if cs.isLost() {
			return nil
		}
		j, err := r.store.Get(ctx, id)
		if err != nil {
			return nil
		}
		switch {
		case j.Ctl == CtlCancel:
			r.finalize(ctx, j, token, StateCanceled, "")
			return nil
		case j.State == StatePaused:
			continue
		case j.State.IsTerminal():
			return nil
		default:
			return j // resumed (running or previewing)
		}
	}
}

// finalize writes a terminal state (fence-guarded) and the completion audit
// entry (§5.11: audit written at job create + completion).
func (r *Runner) finalize(ctx context.Context, j *Job, token int64, state State, errMsg string) {
	fields := map[string]string{
		"state":       string(state),
		"finished_at": fmtTime(time.Now()),
		"ctl":         "",
	}
	if errMsg != "" {
		fields["error"] = errMsg
	}
	ok, err := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{Fields: fields})
	if err != nil || !ok {
		if err != nil && ctx.Err() == nil {
			r.logf("asynqmon: jobs: finalizing %s: %v", j.ID, err)
		}
		return
	}
	fresh, err := r.store.Get(ctx, j.ID)
	if err != nil {
		fresh = j
	}
	if aerr := r.store.AppendAudit(ctx, AuditEntry{
		Event:        AuditJobFinished,
		Actor:        fresh.Actor,
		Verb:         string(fresh.Verb),
		Scope:        fresh.Scope,
		Reason:       fresh.Reason,
		JobID:        fresh.ID,
		PreviewCount: fresh.Counts.Candidates,
		Acted:        fresh.Counts.Acted,
		Skipped:      fresh.Counts.Skipped,
		Failed:       fresh.Counts.Failed,
	}); aerr != nil && ctx.Err() == nil {
		r.logf("asynqmon: jobs: audit for %s: %v", j.ID, aerr)
	}
}

// ----------------------------------------------------------------------------
// Preview: chunked enumeration with persisted cursor
// ----------------------------------------------------------------------------

// enumerate walks the scope in pages, appending matching candidate refs and
// streaming counts/cursor into the job hash after every page. Modeled on the
// search endpoint's scanMatchingTasks, but resumable: the queue list is
// frozen into the job on first run and the (queue, group, page) cursor is
// persisted per batch, so a restarted replica claims and continues instead
// of rescanning (§5.4). Returns true when enumeration ran to completion.
func (r *Runner) enumerate(ctx context.Context, j *Job, token int64, cs *claimSession) bool {
	// First run: freeze the queue list + price the cost class.
	if j.cursorQueues == nil {
		queues, err := r.resolveQueues(j.Scope.Queue)
		if err != nil {
			r.finalize(ctx, j, token, StateFailed, "resolving queues: "+err.Error())
			return false
		}
		j.cursorQueues = queues
		fields := map[string]string{"cursor_queues": mustJSON(queues)}
		if j.CostClass == CostListRemoval {
			listLen, lerr := r.costListLen(ctx, j.Scope, queues)
			if lerr == nil {
				j.CostListLen = listLen
				fields["cost_list_len"] = strconv.FormatInt(listLen, 10)
			}
		}
		if ok, werr := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{Fields: fields}); werr != nil || !ok {
			return false
		}
	}

	// Resume point: the persisted (queue, group, page) cursor applies only to
	// the first queue/group iterated; every later position starts fresh.
	startQ, startG, startPage := j.cursorQIdx, j.cursorGIdx, j.cursorPage
	if startPage < 1 {
		startPage = 1
	}

	for qi := startQ; qi < len(j.cursorQueues); qi++ {
		qname := j.cursorQueues[qi]

		// Group iteration (aggregating state only): the group list is frozen
		// into the cursor when entering a queue so a resumed claim walks the
		// same ordered list.
		groups := []string{""}
		if j.Scope.State == "aggregating" {
			if qi == startQ && j.cursorGroups != nil {
				groups = j.cursorGroups
			} else {
				ginfos, gerr := r.insp.Groups(qname)
				if gerr != nil {
					if errors.Is(gerr, asynq.ErrQueueNotFound) {
						continue // queue removed mid-scan; skip it
					}
					r.logf("asynqmon: jobs: %s: listing groups of %s: %v", j.ID, qname, gerr)
					return false
				}
				groups = make([]string, 0, len(ginfos))
				for _, g := range ginfos {
					groups = append(groups, g.Group)
				}
				sort.Strings(groups)
				if ok, werr := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{Fields: map[string]string{
					"cursor_qidx":   strconv.Itoa(qi),
					"cursor_groups": mustJSON(groups),
					"cursor_gidx":   "0",
					"cursor_page":   "1",
				}}); werr != nil || !ok {
					return false
				}
			}
		}

		giStart := 0
		if qi == startQ && startG < len(groups) {
			giStart = startG
		}
		for gi := giStart; gi < len(groups); gi++ {
			gname := groups[gi]
			page := 1
			if qi == startQ && gi == giStart {
				page = startPage
			}
			for {
				select {
				case <-ctx.Done():
					return false
				default:
				}
				if cs.isLost() {
					return false
				}

				batch, lerr := r.listPage(qname, gname, j.Scope.State, page, r.cfg.PreviewBatch)
				if lerr != nil {
					if errors.Is(lerr, asynq.ErrQueueNotFound) {
						break // queue removed mid-scan; skip it
					}
					r.logf("asynqmon: jobs: %s: listing %s/%s page %d: %v", j.ID, qname, j.Scope.State, page, lerr)
					return false
				}

				var refs []string
				var sample []SampleRow
				for _, ti := range batch {
					if !j.Scope.Matches(ti) {
						continue
					}
					refs = append(refs, ti.Queue+candidateSep+ti.ID)
					if len(sample) < sampleSize { // Lua caps the stored total at 10
						sample = append(sample, SampleRow{
							ID:      ti.ID,
							Queue:   ti.Queue,
							Type:    ti.Type,
							Payload: samplePayload(ti.Payload),
						})
					}
				}
				j.Counts.Candidates += int64(len(refs))

				last := len(batch) < r.cfg.PreviewBatch
				nextQ, nextG, nextPage := qi, gi, page+1
				if last {
					nextG, nextPage = gi+1, 1
					if nextG >= len(groups) {
						nextQ, nextG = qi+1, 0
					}
				}
				ok, werr := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{
					Fields: map[string]string{
						"candidates":  strconv.FormatInt(j.Counts.Candidates, 10),
						"cursor_qidx": strconv.Itoa(nextQ),
						"cursor_gidx": strconv.Itoa(nextG),
						"cursor_page": strconv.Itoa(nextPage),
					},
					Candidates: refs,
					Sample:     sample,
				})
				if werr != nil {
					if ctx.Err() == nil {
						r.logf("asynqmon: jobs: %s: persisting preview batch: %v", j.ID, werr)
					}
					return false
				}
				if !ok {
					r.logf("asynqmon: jobs: %s: fencing token superseded during preview; standing down", j.ID)
					return false
				}

				var cont bool
				if j, cont = r.checkCtl(ctx, j, token, cs); !cont {
					return false
				}
				if last {
					break
				}
				page = nextPage
				select {
				case <-ctx.Done():
					return false
				case <-time.After(r.cfg.PreviewSleep):
				}
			}
		}
	}

	ok, err := r.store.CompletePreview(ctx, j.ID, token)
	if err != nil || !ok {
		if err != nil && ctx.Err() == nil {
			r.logf("asynqmon: jobs: %s: completing preview: %v", j.ID, err)
		}
		return false
	}
	return true
}

// resolveQueues mirrors the search endpoint: ""/"all" means every queue, in
// sorted (stable-pagination) order.
func (r *Runner) resolveQueues(queueParam string) ([]string, error) {
	if queueParam != "" && queueParam != "all" {
		return []string{queueParam}, nil
	}
	qnames, err := r.insp.Queues()
	if err != nil {
		return nil, err
	}
	sort.Strings(qnames)
	return qnames, nil
}

// costListLen prices the list_removal cost class: one LLEN per affected
// LIST, fetched once at preview start (§5.4).
func (r *Runner) costListLen(ctx context.Context, scope Scope, queues []string) (int64, error) {
	keyFn := asynqPendingKey
	if scope.State == "active" {
		keyFn = asynqActiveKey
	}
	pipe := r.cfg.RedisClient.Pipeline()
	cmds := make([]*redis.IntCmd, len(queues))
	for i, q := range queues {
		cmds[i] = pipe.LLen(ctx, keyFn(q))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, err
	}
	var total int64
	for _, c := range cmds {
		n, _ := c.Result()
		total += n
	}
	return total, nil
}

// listPage lists one page of tasks for the given state (gname only for
// aggregating).
func (r *Runner) listPage(qname, gname, state string, page, size int) ([]*asynq.TaskInfo, error) {
	opts := []asynq.ListOption{asynq.Page(page), asynq.PageSize(size)}
	switch state {
	case "active":
		return r.insp.ListActiveTasks(qname, opts...)
	case "pending":
		return r.insp.ListPendingTasks(qname, opts...)
	case "scheduled":
		return r.insp.ListScheduledTasks(qname, opts...)
	case "retry":
		return r.insp.ListRetryTasks(qname, opts...)
	case "archived":
		return r.insp.ListArchivedTasks(qname, opts...)
	case "completed":
		return r.insp.ListCompletedTasks(qname, opts...)
	case "aggregating":
		return r.insp.ListAggregatingTasks(qname, gname, opts...)
	default:
		return nil, fmt.Errorf("unsupported task state %q", state)
	}
}

// samplePayload trims a payload for the 10-row sample (1 KB cap, valid-UTF8
// concerns deferred to the UI which renders it as text).
func samplePayload(p []byte) string {
	const limit = 1024
	if len(p) > limit {
		return string(p[:limit]) + "…"
	}
	return string(p)
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ----------------------------------------------------------------------------
// Execute: act in chunks with per-task re-verification
// ----------------------------------------------------------------------------

// execute acts on the enumerated candidates in chunks of ExecBatch: each
// task is RE-FETCHED (GetTaskInfo) and RE-MATCHED against the scope
// immediately before acting — a task that changed since preview counts as
// skipped, never acted on (§4.3 step 5). Per-item failures are recorded
// (capped; overflow counted). Inter-batch sleep derives from the throttle.
func (r *Runner) execute(ctx context.Context, j *Job, token int64, cs *claimSession) {
	throttle := j.Throttle
	if throttle <= 0 {
		throttle = 200
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if cs.isLost() {
			return
		}

		refs, err := r.store.Candidates(ctx, j.ID, j.actCursor, int64(r.cfg.ExecBatch))
		if err != nil {
			if ctx.Err() == nil {
				r.logf("asynqmon: jobs: %s: reading candidates: %v", j.ID, err)
			}
			return
		}
		if len(refs) == 0 {
			r.finalize(ctx, j, token, StateDone, "")
			return
		}

		var failures []ItemFailure
		for _, ref := range refs {
			qname, taskID := ref[0], ref[1]
			ti, gerr := r.insp.GetTaskInfo(qname, taskID)
			if gerr != nil {
				if errors.Is(gerr, asynq.ErrTaskNotFound) || errors.Is(gerr, asynq.ErrQueueNotFound) {
					// Gone between preview and act — no longer matches.
					j.Counts.Skipped++
					continue
				}
				j.Counts.Failed++
				failures = append(failures, ItemFailure{Queue: qname, ID: taskID, Error: "re-fetch: " + gerr.Error()})
				continue
			}
			if !j.Scope.Matches(ti) {
				j.Counts.Skipped++
				continue
			}
			if aerr := r.act(j.Verb, qname, taskID); aerr != nil {
				j.Counts.Failed++
				failures = append(failures, ItemFailure{Queue: qname, ID: taskID, Error: aerr.Error()})
			} else {
				j.Counts.Acted++
			}
		}
		j.actCursor += int64(len(refs))

		ok, werr := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{
			Fields: map[string]string{
				"acted":      strconv.FormatInt(j.Counts.Acted, 10),
				"skipped":    strconv.FormatInt(j.Counts.Skipped, 10),
				"failed":     strconv.FormatInt(j.Counts.Failed, 10),
				"act_cursor": strconv.FormatInt(j.actCursor, 10),
			},
			Failures: failures,
		})
		if werr != nil {
			if ctx.Err() == nil {
				r.logf("asynqmon: jobs: %s: persisting execute batch: %v", j.ID, werr)
			}
			return
		}
		if !ok {
			r.logf("asynqmon: jobs: %s: fencing token superseded during execute; standing down", j.ID)
			return
		}

		var cont bool
		if j, cont = r.checkCtl(ctx, j, token, cs); !cont {
			return
		}

		// Inter-batch throttle: a chunk of N at T tasks/sec sleeps N/T.
		sleep := time.Duration(float64(len(refs)) / float64(throttle) * float64(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

// act applies the verb via the Inspector (§7: all mutations via public
// Inspector APIs, never key writes).
func (r *Runner) act(v Verb, qname, taskID string) error {
	switch v {
	case VerbRun:
		return r.insp.RunTask(qname, taskID)
	case VerbArchive:
		return r.insp.ArchiveTask(qname, taskID)
	case VerbDelete:
		return r.insp.DeleteTask(qname, taskID)
	case VerbCancel:
		// Cooperative pub/sub signal; no delivery guarantee (§7.6). The UI
		// discloses the listener caveat before execution.
		return r.insp.CancelProcessing(taskID)
	default:
		return fmt.Errorf("unsupported verb %q", v)
	}
}
