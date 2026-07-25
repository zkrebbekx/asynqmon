package hygiene

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/internal/leasefence"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - Engine: report generation (Run) plus the leased interval scheduler
//     (§5.13 singleton-role pattern — asynqmon:lock:hygiene)
//   - webhook delivery: one POST of the report JSON per generation,
//     best-effort, 10s timeout, NO retries (v1 — documented on Options)
// ****************************************************************************

const (
	defaultTickInterval = 30 * time.Second
	defaultLeaseTTL     = 15 * time.Second
	webhookTimeout      = 10 * time.Second
)

// Config configures an Engine. RedisClient and Inspector are required.
type Config struct {
	// RedisClient is the shared connection to the asynq Redis. Required.
	RedisClient redis.UniversalClient

	// Inspector supplies SchedulerEntries()/Servers() for the scheduler-
	// health report. Required.
	Inspector *asynq.Inspector

	// Snapshots reads the stats snapshot cache (wired to stats.Engine.Read).
	// Optional: when nil (stats engine disabled), inventory/storage/
	// dead-letter generation returns ErrStatsUnavailable; scheduler-health
	// still works.
	Snapshots func(ctx context.Context) (*stats.ReadResult, error)

	// WebhookURL, when set, receives a POST of the full report JSON after
	// every generation (scheduled AND Run-now). One attempt, 10s timeout,
	// no retries; failures are logged.
	WebhookURL string

	// TickInterval is the scheduler's due-check cadence. Default 30s.
	TickInterval time.Duration

	// LeaseTTL of the singleton scheduler lease; renewed at ~1/3 TTL.
	// Default 15s. Exposed for tests.
	LeaseTTL time.Duration

	// SchedulerGoneAfter is the scheduler-health GONE threshold (§5.12): a
	// snapshot with no live counterpart silent longer than this is GONE.
	// Default stats.DefaultSchedulerGoneAfter (30s). Exposed for tests.
	SchedulerGoneAfter time.Duration

	// InstanceID identifies this replica in the lease. Default
	// "hostname:pid:<random>".
	InstanceID string

	// HTTPClient posts webhooks. Default: 10s-timeout client.
	HTTPClient *http.Client

	// Now supplies the clock (injected by tests). Default time.Now.
	Now func() time.Time

	// Logf logs scheduler lifecycle events and errors. Default log.Printf.
	Logf func(format string, args ...interface{})
}

// Engine generates hygiene reports on demand (Run) and on schedule (the
// leased tick loop). Report reads are engine-independent (ReadReport /
// ReadConfigs) so any replica serves them.
type Engine struct {
	cfg  Config
	rc   redis.UniversalClient
	insp *asynq.Inspector
	http *http.Client
	now  func() time.Time
	logf func(format string, args ...interface{})

	holding int32 // atomic; 1 while this replica holds the scheduler lease

	// fence is the shared lease+fencing-token helper (§5.13, phase 12);
	// token is minted at acquisition (atomic; 0 while not holding). The
	// SCHEDULED report persist CASes on it; Run-now writes unfenced.
	fence *leasefence.Fence
	token int64 // atomic

	// runMu serializes report generation on this replica: a scheduled pass
	// and a Run-now must not interleave their bounded read bursts.
	runMu sync.Mutex

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewEngine creates an Engine. It does not touch Redis until Start or Run.
func NewEngine(cfg Config) *Engine {
	if cfg.RedisClient == nil {
		panic("hygiene.NewEngine: RedisClient is required")
	}
	if cfg.Inspector == nil {
		panic("hygiene.NewEngine: Inspector is required")
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	if cfg.SchedulerGoneAfter <= 0 {
		cfg.SchedulerGoneAfter = stats.DefaultSchedulerGoneAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: webhookTimeout}
	}
	return &Engine{
		cfg:   cfg,
		rc:    cfg.RedisClient,
		insp:  cfg.Inspector,
		http:  hc,
		now:   cfg.Now,
		logf:  logf,
		fence: newSchedulerFence(cfg.RedisClient),
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

// Start launches the lease and tick loops. Non-blocking; a second Start
// before Stop is a no-op.
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return
	}
	e.started = true
	ctx, e.cancel = context.WithCancel(ctx)
	e.wg.Add(2)
	go e.leaseLoop(ctx)
	go e.tickLoop(ctx)
}

// Stop cancels both loops, waits for them, and best-effort releases the
// lease so a standby replica takes over immediately. Safe to call twice.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return
	}
	e.started = false
	cancel := e.cancel
	e.mu.Unlock()

	cancel()
	e.wg.Wait()

	if atomic.LoadInt32(&e.holding) == 1 {
		ctx, cancelRelease := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelRelease()
		if err := e.fence.Release(ctx, e.cfg.InstanceID); err != nil {
			e.logf("asynqmon: hygiene: releasing scheduler lease: %v", err)
		}
		atomic.StoreInt32(&e.holding, 0)
		atomic.StoreInt64(&e.token, 0)
	}
}

// LeaseHeld reports whether this replica currently holds the scheduler lease.
func (e *Engine) LeaseHeld() bool { return atomic.LoadInt32(&e.holding) == 1 }

// InstanceID returns this replica's lease identity.
func (e *Engine) InstanceID() string { return e.cfg.InstanceID }

func (e *Engine) leaseLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		e.leaseTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) leaseTick(ctx context.Context) {
	if atomic.LoadInt32(&e.holding) == 1 {
		ok, err := e.fence.Renew(ctx, e.cfg.InstanceID, e.cfg.LeaseTTL)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				e.logf("asynqmon: hygiene: renewing scheduler lease: %v", err)
			} else if !ok {
				e.logf("asynqmon: hygiene: scheduler lease lost; standing by")
			}
			atomic.StoreInt32(&e.holding, 0)
			atomic.StoreInt64(&e.token, 0)
		}
		return
	}
	token, err := e.fence.Acquire(ctx, e.cfg.InstanceID, e.cfg.LeaseTTL)
	if err != nil {
		if ctx.Err() == nil {
			e.logf("asynqmon: hygiene: acquiring scheduler lease: %v", err)
		}
		return
	}
	if token > 0 {
		atomic.StoreInt64(&e.token, token)
		atomic.StoreInt32(&e.holding, 1)
		e.logf("asynqmon: hygiene: acquired scheduler lease as %s", e.cfg.InstanceID)
	}
}

func (e *Engine) tickLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if atomic.LoadInt32(&e.holding) != 1 {
			continue
		}
		if err := e.SchedulePass(ctx); err != nil && ctx.Err() == nil {
			e.logf("asynqmon: hygiene: schedule pass failed: %v", err)
		}
	}
}

// ErrSuperseded is returned by a scheduled report persist whose fencing
// token was rejected: a newer claimant holds the scheduler role (§5.13).
var ErrSuperseded = errors.New("hygiene: fencing token superseded; another replica now holds the scheduler role")

// SchedulePass generates every enabled report whose interval has elapsed
// since its last generation. Exported for tests and operational tooling;
// the tick loop calls it on the lease holder only. Scheduled persists are
// fence-guarded with the holder's token; a supersession stops the pass.
func (e *Engine) SchedulePass(ctx context.Context) error {
	configs, err := ReadConfigs(ctx, e.rc)
	if err != nil {
		return err
	}
	lastGen, err := readLastGenerated(ctx, e.rc)
	if err != nil {
		return err
	}
	now := e.now()
	token := atomic.LoadInt64(&e.token)
	for _, kind := range Kinds() {
		cfg := configs[kind]
		if !cfg.Enabled {
			continue
		}
		if now.Sub(lastGen[kind]) < time.Duration(cfg.IntervalSeconds)*time.Second {
			continue
		}
		if _, err := e.run(ctx, kind, token); err != nil {
			if errors.Is(err, ErrSuperseded) {
				// Stand down: a newer scheduler owns the cadence now.
				atomic.StoreInt32(&e.holding, 0)
				atomic.StoreInt64(&e.token, 0)
				return err
			}
			// One report failing (e.g. stats not ready) must not starve the
			// others; log and continue.
			e.logf("asynqmon: hygiene: scheduled %s report failed: %v", kind, err)
		}
	}
	return nil
}

// Run generates one report NOW, persists it as the latest, and posts it to
// the webhook when configured. Serves both the scheduler and the Run-now
// endpoint (any replica may run it — generation only reads asynq state and
// writes asynqmon-owned report keys, so it is safe in read-only mode too).
// Run-now writes are explicitly unfenced (token 0): the operator asked this
// replica to generate, lease or not.
func (e *Engine) Run(ctx context.Context, kind string) (*Report, error) {
	return e.run(ctx, kind, 0)
}

func (e *Engine) run(ctx context.Context, kind string, token int64) (*Report, error) {
	if !ValidKind(kind) {
		return nil, fmt.Errorf("unknown hygiene report kind %q", kind)
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()

	now := e.now()
	rep := &Report{Kind: kind, GeneratedAt: now}

	switch kind {
	case KindSchedulerHealth:
		sh, err := gatherSchedulerHealth(ctx, e.rc, e.insp, now, e.cfg.SchedulerGoneAfter)
		if err != nil {
			return nil, err
		}
		rep.SchedulerHealth = sh
		rep.Summary = schedulerHealthSummary(sh)
	default:
		snaps, asOf, err := e.readSnapshots(ctx)
		if err != nil {
			return nil, err
		}
		rep.DataAsOf = &asOf
		switch kind {
		case KindInventory:
			inv, err := gatherInventory(ctx, e.rc, snaps, now)
			if err != nil {
				return nil, err
			}
			rep.Inventory = inv
			rep.Summary = inventorySummary(inv)
		case KindStorage:
			st, err := gatherStorage(ctx, e.rc, snaps, now)
			if err != nil {
				return nil, err
			}
			rep.Storage = st
			rep.Summary = storageSummary(st)
		case KindDeadLetter:
			dl, err := gatherDeadLetter(ctx, e.rc, snaps, now)
			if err != nil {
				return nil, err
			}
			rep.DeadLetter = dl
			rep.Summary = deadLetterSummary(dl)
		}
	}

	ok, err := persistReport(ctx, e.fence, token, rep)
	if err != nil {
		return nil, fmt.Errorf("persisting %s report: %w", kind, err)
	}
	if !ok {
		return nil, ErrSuperseded
	}
	e.deliverWebhook(rep)
	return rep, nil
}

// readSnapshots resolves the stats snapshot cache, translating every absence
// (nil hook, engine not ready) into ErrStatsUnavailable.
func (e *Engine) readSnapshots(ctx context.Context) ([]*stats.QueueSnapshot, time.Time, error) {
	if e.cfg.Snapshots == nil {
		return nil, time.Time{}, ErrStatsUnavailable
	}
	res, err := e.cfg.Snapshots(ctx)
	if err != nil || res == nil || res.Fleet == nil {
		return nil, time.Time{}, ErrStatsUnavailable
	}
	return res.Queues, res.Fleet.RefreshedAt, nil
}

// deliverWebhook POSTs the report JSON. Best-effort: one attempt, bounded
// timeout, failures logged — NO retries in v1 (documented on Options); a
// missed delivery is recoverable because the report stays readable in-UI.
// Synchronous by design: generation is already off the request hot path for
// scheduled runs, and Run-now callers get delivery-before-response.
func (e *Engine) deliverWebhook(rep *Report) {
	if e.cfg.WebhookURL == "" {
		return
	}
	body, err := json.Marshal(rep)
	if err != nil {
		e.logf("asynqmon: hygiene: encoding %s report for webhook: %v", rep.Kind, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		e.logf("asynqmon: hygiene: building webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := e.http.Do(req)
	if err != nil {
		e.logf("asynqmon: hygiene: webhook delivery of %s report failed (no retry): %v", rep.Kind, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.logf("asynqmon: hygiene: webhook answered %d for %s report (no retry)", resp.StatusCode, rep.Kind)
	}
}
