package stats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
//   - Engine: the leased background sweeper plus the snapshot read facade
//   - the §5.1 per-queue hot read (pipelined, ~14-15 commands per queue)
// ****************************************************************************

// ErrNotReady is returned by Read when no sweep has ever published to the
// cache (sweeper still warming up, disabled everywhere, or dead long enough
// for the cache TTL to expire). Handlers translate this into a 503 rather
// than serving fabricated zeros.
var ErrNotReady = errors.New("stats: fleet cache is empty; no sweep has completed yet")

// Read sources, reported so responses can say where their numbers came from.
const (
	SourceLocal = "local" // in-process copy on the replica holding the lease
	SourceCache = "cache" // shared Redis cache (standby replica, or stale local)
)

const (
	defaultInterval    = 5 * time.Second
	defaultLeaseTTL    = 15 * time.Second
	defaultOrphanGrace = 30 * time.Second // one asynq worker lease interval
	defaultBatchSize   = 200
)

// Config configures an Engine. RedisClient and Inspector are required; every
// other field has a sensible default.
type Config struct {
	// RedisClient is the shared connection to the asynq Redis. Required.
	RedisClient redis.UniversalClient

	// Inspector supplies Servers() — the only data in the sweep that cannot
	// be derived from queue keys (consumer counts, worker totals). Required.
	Inspector *asynq.Inspector

	// Interval between sweeps. Default 5s (§5.1 tier-1 cadence).
	Interval time.Duration

	// LeaseTTL of the singleton sweeper lease; renewed at ~1/3 TTL.
	// Default 15s. Exposed for tests.
	LeaseTTL time.Duration

	// OrphanGrace is how long past lease expiry an active task must be to
	// count as an orphan candidate. Default 30s (one asynq lease interval).
	OrphanGrace time.Duration

	// BatchSize is the number of queues read per Redis pipeline, bounding
	// pipeline sizes (~14 commands per queue per pipeline). Default 200.
	BatchSize int

	// CacheTTL for cached snapshot hashes. Default max(10*Interval, 2m).
	CacheTTL time.Duration

	// InstanceID identifies this replica in the lease. Default
	// "hostname:pid:<random>".
	InstanceID string

	// Logf logs sweeper lifecycle events and errors. Default log.Printf.
	Logf func(format string, args ...interface{})
}

// Engine runs the stats sweep on the replica that wins the lease and serves
// snapshot reads on every replica: in-process copy when fresh (the sweeping
// replica), shared Redis cache otherwise (§5.13 read model).
//
// Tier-1 scope (§5.1 tiering table, §6 phase 2): the sweep reads every queue
// every interval, which is within the command budget up to ~2k queues. There
// is deliberately no rotating shard / hot-set logic yet — that is phase 12.
// Beyond tier-1 fleet sizes the sweep still works, just costs more than the
// budget; every row carries RefreshedAt so coverage stays honest regardless.
type Engine struct {
	cfg  Config
	rc   redis.UniversalClient
	insp *asynq.Inspector
	logf func(format string, args ...interface{})

	mem  *memoryStore
	kick chan struct{} // signals the sweep loop right after lease acquisition

	holding int32 // atomic; 1 while this replica holds the sweeper lease

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewEngine creates an Engine. It does not touch Redis until Start (or
// SweepNow) is called.
func NewEngine(cfg Config) *Engine {
	if cfg.RedisClient == nil {
		panic("stats.NewEngine: RedisClient is required")
	}
	if cfg.Inspector == nil {
		panic("stats.NewEngine: Inspector is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.OrphanGrace <= 0 {
		cfg.OrphanGrace = defaultOrphanGrace
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 10 * cfg.Interval
		if cfg.CacheTTL < 2*time.Minute {
			cfg.CacheTTL = 2 * time.Minute
		}
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Engine{
		cfg:  cfg,
		rc:   cfg.RedisClient,
		insp: cfg.Inspector,
		logf: logf,
		mem:  newMemoryStore(),
		kick: make(chan struct{}, 1),
	}
}

// defaultInstanceID builds "hostname:pid:<random>" — unique per process so
// two replicas on one host (or a restarted process racing its old lease)
// never mistake each other's lease for their own.
func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a time-based suffix; uniqueness only needs to hold
		// across concurrently-live replicas.
		return fmt.Sprintf("%s:%d:%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Start launches the lease and sweep loops. It is non-blocking and safe to
// call once per Stop; a second Start before Stop is a no-op.
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
	go e.sweepLoop(ctx)
}

// Stop cancels both loops, waits for them to exit (no goroutine leaks), and
// best-effort releases the lease so a standby replica takes over immediately
// instead of waiting out the lease TTL. Safe to call multiple times.
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
		if err := releaseLease(ctx, e.rc, e.cfg.InstanceID); err != nil {
			// The TTL is the backstop; a failed release only delays takeover.
			e.logf("asynqmon: stats: releasing sweeper lease: %v", err)
		}
		atomic.StoreInt32(&e.holding, 0)
	}
}

// LeaseHeld reports whether this replica currently holds the sweeper lease.
func (e *Engine) LeaseHeld() bool { return atomic.LoadInt32(&e.holding) == 1 }

// InstanceID returns this replica's lease identity.
func (e *Engine) InstanceID() string { return e.cfg.InstanceID }

// LastSweep returns the measured cost of the most recent local sweep
// (zero-valued if this replica has never swept).
func (e *Engine) LastSweep() SweepStats { return e.mem.sweepStats() }

// leaseLoop tries to hold the sweeper lease: acquire when free, renew at
// ~1/3 TTL while held. On any renewal failure — lost or errored — it
// conservatively drops to standby; a transient Redis blip then costs one
// lease TTL of sweep downtime, which the cache TTL comfortably absorbs.
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
		ok, err := renewLease(ctx, e.rc, e.cfg.InstanceID, e.cfg.LeaseTTL)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				e.logf("asynqmon: stats: renewing sweeper lease: %v", err)
			} else if !ok {
				e.logf("asynqmon: stats: sweeper lease lost; standing by")
			}
			atomic.StoreInt32(&e.holding, 0)
		}
		return
	}
	ok, err := tryAcquireLease(ctx, e.rc, e.cfg.InstanceID, e.cfg.LeaseTTL)
	if err != nil {
		if ctx.Err() == nil {
			e.logf("asynqmon: stats: acquiring sweeper lease: %v", err)
		}
		return
	}
	if ok {
		atomic.StoreInt32(&e.holding, 1)
		e.logf("asynqmon: stats: acquired sweeper lease as %s", e.cfg.InstanceID)
		// Sweep immediately on takeover instead of waiting out an interval,
		// so a fresh deployment (or failover) serves data right away.
		select {
		case e.kick <- struct{}{}:
		default:
		}
	}
}

// sweepLoop runs a sweep every interval while this replica holds the lease.
func (e *Engine) sweepLoop(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.kick:
		}
		if atomic.LoadInt32(&e.holding) != 1 {
			continue
		}
		if err := e.sweep(ctx); err != nil && ctx.Err() == nil {
			// Keep looping: a failed sweep leaves the previous snapshots in
			// place with their honest (aging) RefreshedAt stamps.
			e.logf("asynqmon: stats: sweep failed: %v", err)
		}
	}
}

// SweepNow runs a single sweep immediately, regardless of lease ownership.
// Exposed for tests and operational tooling; running it on a standby replica
// costs only duplicate reads (cache writes are idempotent, last-writer-wins,
// and every row is stamped with its own RefreshedAt).
func (e *Engine) SweepNow(ctx context.Context) error { return e.sweep(ctx) }

// sweep performs one full tier-1 pass: enumerate queues, pipeline the §5.1
// hot read per queue, join server data, publish to memory + Redis cache.
func (e *Engine) sweep(ctx context.Context) error {
	begin := time.Now()
	reads := 0

	qnames, err := e.rc.SMembers(ctx, allQueuesKey).Result()
	reads++
	if err != nil {
		return fmt.Errorf("reading queue set: %w", err)
	}
	sort.Strings(qnames)

	// Previous cache index, for detecting queues deleted since the last
	// sweep (possibly a sweep by another replica — the index is shared, so
	// handovers do not leak ghost rows).
	prevIndex, err := e.rc.SMembers(ctx, queueIndexKey).Result()
	reads++
	if err != nil {
		return fmt.Errorf("reading cache index: %w", err)
	}

	// One Servers() call per sweep: O(servers), the only non-queue-key data
	// in the snapshot. If it fails, abort the whole sweep rather than
	// publishing Consumers=0 for every queue — fabricated "zero consumer"
	// alarms are worse than visibly stale data.
	servers, err := e.insp.Servers()
	if err != nil {
		return fmt.Errorf("reading servers: %w", err)
	}
	consumers := make(map[string]int)
	workersTotal, workersBusy := 0, 0
	for _, s := range servers {
		workersTotal += s.Concurrency
		workersBusy += len(s.ActiveWorkers)
		for q := range s.Queues {
			consumers[q]++
		}
	}

	now := time.Now()
	snaps := make(map[string]*QueueSnapshot, len(qnames))
	for i := 0; i < len(qnames); i += e.cfg.BatchSize {
		end := i + e.cfg.BatchSize
		if end > len(qnames) {
			end = len(qnames)
		}
		n, err := e.sweepBatch(ctx, qnames[i:end], now, consumers, snaps)
		reads += n
		if err != nil {
			return fmt.Errorf("sweeping queues: %w", err)
		}
	}

	fleet := aggregateFleet(snaps, len(servers), workersTotal, workersBusy, now)

	var removed []string
	for _, name := range prevIndex {
		if _, ok := snaps[name]; !ok {
			removed = append(removed, name)
		}
	}

	writes, werr := writeCache(ctx, e.rc, fleet, snaps, removed, e.cfg.CacheTTL)
	// Publish to memory even if the cache write failed: local data is good,
	// and this replica keeps serving it while Redis recovers.
	e.mem.replace(fleet, snaps, SweepStats{
		At:        now,
		Queues:    len(qnames),
		ReadCmds:  reads,
		WriteCmds: writes,
		Duration:  time.Since(begin),
	})
	if werr != nil {
		return fmt.Errorf("writing cache: %w", werr)
	}
	return nil
}

// queueCmds holds the pipelined command handles of one queue's hot read, in
// §5.1 order.
type queueCmds struct {
	pending         *redis.IntCmd
	active          *redis.IntCmd
	scheduled       *redis.IntCmd
	retry           *redis.IntCmd
	archived        *redis.IntCmd
	completed       *redis.IntCmd
	paused          *redis.StringCmd
	processed       *redis.StringCmd
	failed          *redis.StringCmd
	orphans         *redis.IntCmd
	nextRetry       *redis.ZSliceCmd
	pastDue         *redis.IntCmd
	oldestPendingID *redis.StringCmd
	groups          *redis.IntCmd
}

// sweepBatch reads up to BatchSize queues in two pipelined hops and fills
// out. Hop 1 is the fixed 14-command hot read per queue; hop 2 fetches
// pending_since for each queue's oldest pending task (only for queues that
// have one). Returns the number of Redis commands issued.
//
// Per-queue cost: 14 commands, +1 when the queue has pending tasks —
// matching the §5.1 costing of ~14-15 commands/queue.
func (e *Engine) sweepBatch(ctx context.Context, names []string, now time.Time, consumers map[string]int, out map[string]*QueueSnapshot) (int, error) {
	// Lease scores and scheduled scores are Unix seconds (see keys.go).
	orphanCutoff := "(" + strconv.FormatInt(now.Add(-e.cfg.OrphanGrace).Unix(), 10)
	nowSec := strconv.FormatInt(now.Unix(), 10)
	reads := 0

	pipe := e.rc.Pipeline()
	cmds := make([]queueCmds, len(names))
	for i, q := range names {
		c := &cmds[i]
		c.pending = pipe.LLen(ctx, pendingKey(q))
		c.active = pipe.LLen(ctx, activeKey(q))
		c.scheduled = pipe.ZCard(ctx, scheduledKey(q))
		c.retry = pipe.ZCard(ctx, retryKey(q))
		c.archived = pipe.ZCard(ctx, archivedKey(q))
		c.completed = pipe.ZCard(ctx, completedKey(q))
		c.paused = pipe.Get(ctx, pausedKey(q))
		c.processed = pipe.Get(ctx, processedKey(q, now))
		c.failed = pipe.Get(ctx, failedKey(q, now))
		c.orphans = pipe.ZCount(ctx, leaseKey(q), "-inf", orphanCutoff)
		c.nextRetry = pipe.ZRangeWithScores(ctx, retryKey(q), 0, 0)
		c.pastDue = pipe.ZCount(ctx, scheduledKey(q), "-inf", nowSec)
		// pending is a LIST: LPUSH in, RPOPLPUSH out, so the oldest task is
		// at index -1. Its age needs a second hop into the task hash.
		c.oldestPendingID = pipe.LIndex(ctx, pendingKey(q), -1)
		c.groups = pipe.SCard(ctx, allGroupsKey(q))
		reads += 14
	}
	// redis.Nil is expected for missing GETs (unpaused queues, day-one
	// counters) and missing LINDEX; per-command results are still populated,
	// so inspect per command below instead of aborting (task_timing.go
	// establishes this pattern).
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return reads, err
	}

	// Hop 2: pending_since (Unix nanos on the task hash) for oldest-pending age.
	type sinceHop struct {
		q   string
		cmd *redis.StringCmd
	}
	pipe2 := e.rc.Pipeline()
	var hops []sinceHop
	for i, q := range names {
		id, err := cmds[i].oldestPendingID.Result()
		if err != nil || id == "" {
			continue
		}
		hops = append(hops, sinceHop{q: q, cmd: pipe2.HGet(ctx, taskKey(q, id), "pending_since")})
		reads++
	}
	if len(hops) > 0 {
		if _, err := pipe2.Exec(ctx); err != nil && err != redis.Nil {
			return reads, err
		}
	}
	oldest := make(map[string]time.Time, len(hops))
	for _, h := range hops {
		// A miss means the task was dequeued between hops (or an asynq
		// upgrade changed the layout): age degrades to unknown, not an error.
		v, err := h.cmd.Result()
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			oldest[h.q] = time.Unix(0, n)
		}
	}

	for i, q := range names {
		c := &cmds[i]
		s := &QueueSnapshot{
			Queue:              q,
			Consumers:          consumers[q],
			OldestPendingSince: oldest[q],
			RefreshedAt:        now,
		}
		s.Pending, _ = c.pending.Result()
		s.Active, _ = c.active.Result()
		s.Scheduled, _ = c.scheduled.Result()
		s.Retry, _ = c.retry.Result()
		s.Archived, _ = c.archived.Result()
		s.Completed, _ = c.completed.Result()
		s.Groups, _ = c.groups.Result()
		s.OrphanCandidates, _ = c.orphans.Result()
		s.PastDueScheduled, _ = c.pastDue.Result()
		if v, err := c.paused.Result(); err == nil {
			// Existence == paused; the value (pause time, Unix seconds) is a
			// bonus that may not parse under future asynq versions.
			s.Paused = true
			if sec, perr := strconv.ParseInt(v, 10, 64); perr == nil && sec > 0 {
				s.PausedSince = time.Unix(sec, 0)
			}
		}
		if v, err := c.processed.Result(); err == nil {
			s.ProcessedToday = decodeInt(v)
		}
		if v, err := c.failed.Result(); err == nil {
			s.FailedToday = decodeInt(v)
		}
		if zs, err := c.nextRetry.Result(); err == nil && len(zs) > 0 {
			s.NextRetryAt = time.Unix(int64(zs[0].Score), 0)
		}
		out[q] = s
	}
	return reads, nil
}

// aggregateFleet folds queue snapshots plus server-derived totals into the
// fleet snapshot. Paused and zero-consumer queue counts live here because
// they are population counts, not sums — they cannot be reconstructed from
// the summed fields.
func aggregateFleet(snaps map[string]*QueueSnapshot, servers, workersTotal, workersBusy int, now time.Time) *FleetSnapshot {
	f := &FleetSnapshot{
		QueuesTotal:  len(snaps),
		Servers:      servers,
		WorkersTotal: workersTotal,
		WorkersBusy:  workersBusy,
		RefreshedAt:  now,
	}
	for _, s := range snaps {
		f.Pending += s.Pending
		f.Active += s.Active
		f.Scheduled += s.Scheduled
		f.Retry += s.Retry
		f.Archived += s.Archived
		f.Completed += s.Completed
		f.Groups += s.Groups
		f.ProcessedToday += s.ProcessedToday
		f.FailedToday += s.FailedToday
		f.OrphanCandidates += s.OrphanCandidates
		f.PastDueScheduled += s.PastDueScheduled
		if s.Paused {
			f.PausedQueues++
		}
		if s.Consumers == 0 {
			f.ZeroConsumerQueues++
		}
	}
	return f
}

// ReadResult is a consistent view of the fleet: the aggregate and every
// queue snapshot (sorted by name), plus which store served it.
type ReadResult struct {
	Fleet  *FleetSnapshot
	Queues []*QueueSnapshot
	Source string // SourceLocal or SourceCache
}

// Read returns the current snapshots. The in-process copy is preferred (zero
// latency, only populated on the sweeping replica); when it is absent or
// stale — this replica lost the lease, or never held it — Read falls back to
// the shared Redis cache. Snapshots in the result are immutable; callers may
// sort the slice but must not mutate the structs.
func (e *Engine) Read(ctx context.Context) (*ReadResult, error) {
	if fleet, queues := e.mem.get(); fleet != nil && time.Since(fleet.RefreshedAt) <= e.localStaleAfter() {
		return &ReadResult{Fleet: fleet, Queues: queues, Source: SourceLocal}, nil
	}
	fleet, queues, err := readCache(ctx, e.rc, e.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	return &ReadResult{Fleet: fleet, Queues: queues, Source: SourceCache}, nil
}

// localStaleAfter bounds how old the in-process copy may be before reads
// fall through to the shared cache (where a healthier replica may be
// publishing fresher sweeps). Three missed sweeps is decisively unhealthy;
// the 15s floor keeps sub-second test intervals from thrashing the fallback.
func (e *Engine) localStaleAfter() time.Duration {
	d := 3 * e.cfg.Interval
	if d < 15*time.Second {
		d = 15 * time.Second
	}
	return d
}
