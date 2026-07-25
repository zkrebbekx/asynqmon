package errsig

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

	"github.com/hibiken/asynqmon/internal/leasefence"
)

// ****************************************************************************
// This file defines:
//   - Indexer: the leased singleton running both §5.7 feeders
//   - the archived-tail feeder (SweepTailNow): per-queue (score,id) cursor
//     over the archived zset, died-at ascending — exact and incremental
//   - the retry sampler (SampleRetryNow): bounded per-queue window over the
//     retry zset, contributions replaced wholesale per run and flagged
//     sampled — explicitly NOT a cursor (§5.7: retry scores are future-dated
//     next_process_at values, rewritten on every attempt, so a cursor over
//     them would silently skip or double-count; a periodic bounded resample
//     is the only honest read)
//
// Task decode goes through Inspector.GetTaskInfo — the same public-API path
// the AQL executors use — never by decoding asynq's protobuf task messages.
// ****************************************************************************

const (
	defaultTailInterval    = 15 * time.Second // §5.7 feeder 1 cadence
	defaultSampleInterval  = 60 * time.Second // §5.7 feeder 2 cadence
	defaultLeaseTTL        = 15 * time.Second
	defaultMaxTailPerQueue = 1000 // GetTaskInfo budget per queue per sweep
	defaultMaxSamplePer    = 200  // retry window size per queue per sample
	tailBatchSize          = 200  // ZRANGEBYSCORE page size

	// maxRefsPerSigPerRun bounds one run's ref additions per signature; the
	// store's own cap (maxRefsPerSig) bounds the total.
	maxRefsPerSigPerRun = 25
)

// Config configures an Indexer. RedisClient and Inspector are required.
type Config struct {
	RedisClient redis.UniversalClient
	Inspector   *asynq.Inspector

	// TailInterval between archived-tail sweeps. Default 15s.
	TailInterval time.Duration
	// SampleInterval between retry resamples. Default 60s.
	SampleInterval time.Duration
	// LeaseTTL of the singleton indexer lease; renewed at ~1/3 TTL. Default 15s.
	LeaseTTL time.Duration
	// MaxTailPerQueue caps archived entries examined per queue per sweep
	// (the tail resumes from its cursor next sweep). Default 1000.
	MaxTailPerQueue int
	// MaxSamplePerQueue caps retry tasks decoded per queue per sample.
	// Default 200.
	MaxSamplePerQueue int
	// InstanceID identifies this replica in the lease. Default
	// "hostname:pid:<random>".
	InstanceID string
	// Logf logs lifecycle events and errors. Default log.Printf.
	Logf func(format string, args ...interface{})
}

// Indexer runs both feeders on the replica holding the errsig lease; every
// other replica stands by (reads are served from the shared store by any
// replica, indexer running or not).
type Indexer struct {
	cfg  Config
	rc   redis.UniversalClient
	insp *asynq.Inspector
	logf func(format string, args ...interface{})

	kick    chan struct{}
	holding int32

	// fence is the shared lease+fencing-token helper (§5.13, phase 12);
	// token is the fencing token minted at acquisition (atomic; 0 while not
	// holding — and for explicitly off-lease *Now calls, which write
	// unfenced by design). Index writes CAS on it.
	fence *leasefence.Fence
	token int64 // atomic

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewIndexer creates an Indexer. It does not touch Redis until Start (or one
// of the *Now methods) is called.
func NewIndexer(cfg Config) *Indexer {
	if cfg.RedisClient == nil {
		panic("errsig.NewIndexer: RedisClient is required")
	}
	if cfg.Inspector == nil {
		panic("errsig.NewIndexer: Inspector is required")
	}
	if cfg.TailInterval <= 0 {
		cfg.TailInterval = defaultTailInterval
	}
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultSampleInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.MaxTailPerQueue <= 0 {
		cfg.MaxTailPerQueue = defaultMaxTailPerQueue
	}
	if cfg.MaxSamplePerQueue <= 0 {
		cfg.MaxSamplePerQueue = defaultMaxSamplePer
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Indexer{
		cfg:   cfg,
		rc:    cfg.RedisClient,
		insp:  cfg.Inspector,
		logf:  logf,
		kick:  make(chan struct{}, 1),
		fence: newIndexerFence(cfg.RedisClient),
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

// Start launches the lease and work loops (non-blocking; second Start before
// Stop is a no-op).
func (ix *Indexer) Start(ctx context.Context) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.started {
		return
	}
	ix.started = true
	ctx, ix.cancel = context.WithCancel(ctx)
	ix.wg.Add(2)
	go ix.leaseLoop(ctx)
	go ix.workLoop(ctx)
}

// Stop cancels both loops, waits for them, and best-effort releases the
// lease so a standby takes over without waiting out the TTL.
func (ix *Indexer) Stop() {
	ix.mu.Lock()
	if !ix.started {
		ix.mu.Unlock()
		return
	}
	ix.started = false
	cancel := ix.cancel
	ix.mu.Unlock()

	cancel()
	ix.wg.Wait()

	if atomic.LoadInt32(&ix.holding) == 1 {
		ctx, cancelRelease := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelRelease()
		if err := ix.fence.Release(ctx, ix.cfg.InstanceID); err != nil {
			ix.logf("asynqmon: errsig: releasing indexer lease: %v", err)
		}
		atomic.StoreInt32(&ix.holding, 0)
		atomic.StoreInt64(&ix.token, 0)
	}
}

// LeaseHeld reports whether this replica currently holds the indexer lease.
func (ix *Indexer) LeaseHeld() bool { return atomic.LoadInt32(&ix.holding) == 1 }

// InstanceID returns this replica's lease identity.
func (ix *Indexer) InstanceID() string { return ix.cfg.InstanceID }

func (ix *Indexer) leaseLoop(ctx context.Context) {
	defer ix.wg.Done()
	ticker := time.NewTicker(ix.cfg.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		ix.leaseTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (ix *Indexer) leaseTick(ctx context.Context) {
	if atomic.LoadInt32(&ix.holding) == 1 {
		ok, err := ix.fence.Renew(ctx, ix.cfg.InstanceID, ix.cfg.LeaseTTL)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				ix.logf("asynqmon: errsig: renewing indexer lease: %v", err)
			} else if !ok {
				ix.logf("asynqmon: errsig: indexer lease lost; standing by")
			}
			atomic.StoreInt32(&ix.holding, 0)
			atomic.StoreInt64(&ix.token, 0)
		}
		return
	}
	token, err := ix.fence.Acquire(ctx, ix.cfg.InstanceID, ix.cfg.LeaseTTL)
	if err != nil {
		if ctx.Err() == nil {
			ix.logf("asynqmon: errsig: acquiring indexer lease: %v", err)
		}
		return
	}
	if token > 0 {
		atomic.StoreInt64(&ix.token, token)
		atomic.StoreInt32(&ix.holding, 1)
		ix.logf("asynqmon: errsig: acquired indexer lease as %s", ix.cfg.InstanceID)
		select {
		case ix.kick <- struct{}{}:
		default:
		}
	}
}

// workLoop ticks at TailInterval; the sampler piggybacks every SampleInterval
// (both feeders run sequentially on one goroutine — no concurrent writes to
// merge, so read-modify-write of signature hashes stays race-free under the
// single lease).
func (ix *Indexer) workLoop(ctx context.Context) {
	defer ix.wg.Done()
	ticker := time.NewTicker(ix.cfg.TailInterval)
	defer ticker.Stop()
	var lastSample time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-ix.kick:
		}
		if atomic.LoadInt32(&ix.holding) != 1 {
			continue
		}
		if err := ix.SweepTailNow(ctx); err != nil && ctx.Err() == nil {
			ix.logf("asynqmon: errsig: archived tail sweep failed: %v", err)
		}
		if time.Since(lastSample) >= ix.cfg.SampleInterval {
			if err := ix.SampleRetryNow(ctx); err != nil && ctx.Err() == nil {
				ix.logf("asynqmon: errsig: retry sample failed: %v", err)
			}
			lastSample = time.Now()
		}
	}
}

// ----------------------------------------------------------------------------
// Feeder 1: archived tail
// ----------------------------------------------------------------------------

// ErrSuperseded is returned by a feeder run whose fenced index write was
// rejected: a newer claimant minted a higher fencing token, so this replica
// stood down (§5.13).
var ErrSuperseded = errors.New("errsig: fencing token superseded; another replica now holds the indexer role")

// currentToken reads this replica's fencing token (0 = off-lease → unfenced
// writes, the documented *Now semantics).
func (ix *Indexer) currentToken() int64 { return atomic.LoadInt64(&ix.token) }

// standDown clears holder state after a fence rejection so the lease loop
// re-contends instead of retrying doomed writes.
func (ix *Indexer) standDown() {
	atomic.StoreInt32(&ix.holding, 0)
	atomic.StoreInt64(&ix.token, 0)
}

// hsetCmd flattens a field map into one HSET command.
func hsetCmd(key string, fields map[string]interface{}) leasefence.Cmd {
	cmd := make(leasefence.Cmd, 0, 2+2*len(fields))
	cmd = append(cmd, "HSET", key)
	for f, v := range fields {
		cmd = append(cmd, f, v)
	}
	return cmd
}

// tailCursor is one queue's resume point: the last (died-at score, task id)
// examined. Scores are died-at Unix seconds; the archived zset only ever
// gains entries at (or above) the current time, so resuming from the cursor
// score inclusively and skipping already-served lexical ties — the same
// (score,id) discipline as the console's cursor listings — never reprocesses
// and never skips.
type tailCursor struct {
	score int64
	id    string
	valid bool
}

func (c tailCursor) encode() string { return strconv.FormatInt(c.score, 10) + "|" + c.id }

func decodeCursor(s string) tailCursor {
	i := len(s)
	for j := 0; j < len(s); j++ {
		if s[j] == '|' {
			i = j
			break
		}
	}
	if i == len(s) {
		return tailCursor{}
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return tailCursor{}
	}
	return tailCursor{score: n, id: s[i+1:], valid: true}
}

// tailAccum is one signature's in-sweep accumulation.
type tailAccum struct {
	template string
	counts   map[[2]string]int64 // (queue, type) → new archived observations
	first    time.Time
	last     time.Time
	refs     []redis.Z
}

// SweepTailNow runs one archived-tail sweep across every queue, regardless
// of lease ownership (exposed for tests and operational tooling; running it
// off-lease only costs duplicate reads and idempotent merges).
func (ix *Indexer) SweepTailNow(ctx context.Context) error {
	now := time.Now()

	qnames, err := ix.rc.SMembers(ctx, allQueuesKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading queue set: %w", err)
	}
	sort.Strings(qnames)

	// Per-queue pipeline: archive size (trim detection), today's failed
	// counter (magnitude cross-check), stored cursor.
	type qMeta struct {
		card   *redis.IntCmd
		failed *redis.StringCmd
		cursor *redis.StringCmd
	}
	pipe := ix.rc.Pipeline()
	metas := make([]qMeta, len(qnames))
	for i, q := range qnames {
		metas[i] = qMeta{
			card:   pipe.ZCard(ctx, archivedKey(q)),
			failed: pipe.Get(ctx, FailedTodayKey(q, now)),
			cursor: pipe.HGet(ctx, cursorsKey, q),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading queue metadata: %w", err)
	}

	var failedTotal int64
	var trimmed []string
	agg := make(map[string]*tailAccum)
	newCursors := make(map[string]string)

	for i, q := range qnames {
		if card, err := metas[i].card.Result(); err == nil && card >= ArchiveTrimCap {
			trimmed = append(trimmed, q)
		}
		if v, err := metas[i].failed.Result(); err == nil {
			failedTotal += decodeInt(v)
		}
		cur := tailCursor{}
		if v, err := metas[i].cursor.Result(); err == nil {
			cur = decodeCursor(v)
		}
		newCur, err := ix.tailQueue(ctx, q, cur, agg)
		if err != nil {
			return fmt.Errorf("tailing archived %q: %w", q, err)
		}
		if newCur.valid && newCur != cur {
			newCursors[q] = newCur.encode()
		}
	}

	if err := ix.mergeAndWrite(ctx, now, agg, failedTotal, trimmed, newCursors); err != nil {
		return err
	}
	return nil
}

// tailQueue walks one queue's archived zset from its cursor, died-at
// ascending, decoding up to MaxTailPerQueue entries. Every entry examined —
// including ones whose task vanished (trim/delete) or moved state between
// the ZRANGE and the fetch — advances the cursor, so a poisoned or trimmed
// range can never wedge the tail.
func (ix *Indexer) tailQueue(ctx context.Context, q string, cur tailCursor, agg map[string]*tailAccum) (tailCursor, error) {
	minStr := "-inf"
	resuming := cur.valid
	if resuming {
		minStr = strconv.FormatInt(cur.score, 10) // inclusive; ties filtered below
	}
	newCur := cur
	examined := 0
	var offset int64

	for examined < ix.cfg.MaxTailPerQueue {
		entries, err := ix.rc.ZRangeByScoreWithScores(ctx, archivedKey(q), &redis.ZRangeBy{
			Min: minStr, Max: "+inf", Offset: offset, Count: tailBatchSize,
		}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return newCur, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			if examined >= ix.cfg.MaxTailPerQueue {
				break
			}
			id, _ := e.Member.(string)
			score := int64(e.Score)
			if resuming && score == cur.score && id <= cur.id {
				continue // tie already served by a previous sweep
			}
			examined++
			newCur = tailCursor{score: score, id: id, valid: true}

			ti, gerr := ix.insp.GetTaskInfo(q, id)
			if gerr != nil {
				continue // vanished between ZRANGE and fetch (trimmed/deleted): skip, honestly
			}
			if ti.State != asynq.TaskStateArchived || ti.LastErr == "" {
				continue // moved mid-sweep, or archived without an error message
			}
			template := Normalize(ti.LastErr)
			if template == "" {
				continue
			}
			sig := SigHash(template)
			a := agg[sig]
			if a == nil {
				a = &tailAccum{template: template, counts: make(map[[2]string]int64)}
				agg[sig] = a
			}
			diedAt := time.Unix(score, 0)
			a.counts[[2]string{q, ti.Type}]++
			if a.first.IsZero() || diedAt.Before(a.first) {
				a.first = diedAt
			}
			if diedAt.After(a.last) {
				a.last = diedAt
			}
			if len(a.refs) < maxRefsPerSigPerRun {
				a.refs = append(a.refs, redis.Z{Score: float64(score), Member: TaskRef{Queue: q, ID: id}.member()})
			}
		}
		offset += int64(len(entries))
		if len(entries) < tailBatchSize {
			break
		}
	}
	return newCur, nil
}

// mergeAndWrite folds one tail sweep's accumulations into the store: it
// loads EVERY tracked signature (bounded by maxSignatures), applies the new
// archived observations, rolls every signature's trend snapshot (untouched
// signatures must still roll toward a flat delta, or a dead signature would
// read "growing" forever), and writes back only what changed.
func (ix *Indexer) mergeAndWrite(ctx context.Context, now time.Time, agg map[string]*tailAccum, failedTotal int64, trimmed []string, newCursors map[string]string) error {
	indexSigs, err := ix.rc.ZRange(ctx, indexKey, 0, -1).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading signature index: %w", err)
	}
	all, _, err := readSignatureHashes(ctx, ix.rc, indexSigs)
	if err != nil {
		return fmt.Errorf("reading signature hashes: %w", err)
	}

	writeSet := make(map[string]bool)
	for sig, acc := range agg {
		s := all[sig]
		if s == nil {
			s = &Signature{Sig: sig, Template: acc.template, PrevCount: -1}
			all[sig] = s
		}
		for qt, n := range acc.counts {
			c := s.cell(qt[0], qt[1])
			c.Archived += n
			s.ArchivedCount += n
		}
		if s.FirstSeen.IsZero() || (!acc.first.IsZero() && acc.first.Before(s.FirstSeen)) {
			s.FirstSeen = acc.first
		}
		if acc.last.After(s.LastSeen) {
			s.LastSeen = acc.last
		}
		writeSet[sig] = true
	}
	for sig, s := range all {
		prevSnapAt, prevPrev, prevSnap := s.SnapAt, s.PrevCount, s.SnapCount
		s.rollSnapshot(now)
		if s.SnapAt != prevSnapAt || s.PrevCount != prevPrev || s.SnapCount != prevSnap {
			writeSet[sig] = true
		}
	}

	// Fence-guarded batch (§5.13, phase 12): a superseded ex-holder's merge
	// must never land over a newer holder's index. Token 0 (off-lease
	// SweepTailNow — tests/tooling) writes unfenced by design.
	token := ix.currentToken()
	ttlMs := indexTTL.Milliseconds()
	cmds := make([]leasefence.Cmd, 0, 6*len(writeSet)+8)
	for sig := range writeSet {
		s := all[sig]
		fields, herr := s.toHash()
		if herr != nil {
			return fmt.Errorf("encoding signature %s: %w", sig, herr)
		}
		cmds = append(cmds,
			hsetCmd(sigKey(sig), fields),
			leasefence.Cmd{"PEXPIRE", sigKey(sig), ttlMs},
			leasefence.Cmd{"ZADD", indexKey, float64(s.LastSeen.Unix()), sig})
		if acc := agg[sig]; acc != nil && len(acc.refs) > 0 {
			zadd := leasefence.Cmd{"ZADD", refsKey(sig)}
			for _, r := range acc.refs {
				zadd = append(zadd, r.Score, r.Member)
			}
			cmds = append(cmds, zadd,
				leasefence.Cmd{"ZREMRANGEBYRANK", refsKey(sig), 0, int64(-(maxRefsPerSig + 1))},
				leasefence.Cmd{"PEXPIRE", refsKey(sig), ttlMs})
		}
	}

	trimmedJSON, err := json.Marshal(trimmed)
	if err != nil {
		return fmt.Errorf("encoding trimmed queues: %w", err)
	}
	cmds = append(cmds,
		leasefence.Cmd{"HSET", metaKey,
			"failed_today_total", strconv.FormatInt(failedTotal, 10),
			"trimmed_queues", string(trimmedJSON),
			"tail_at", strconv.FormatInt(now.Unix(), 10)},
		leasefence.Cmd{"PEXPIRE", metaKey, ttlMs})
	if len(newCursors) > 0 {
		hset := leasefence.Cmd{"HSET", cursorsKey}
		for q, v := range newCursors {
			hset = append(hset, q, v)
		}
		cmds = append(cmds, hset)
	}
	cmds = append(cmds,
		leasefence.Cmd{"PEXPIRE", cursorsKey, ttlMs},
		// TTL prune (90d, mirroring the archive) + population cap.
		leasefence.Cmd{"ZREMRANGEBYSCORE", indexKey, "-inf", strconv.FormatInt(now.Add(-indexTTL).Unix(), 10)},
		leasefence.Cmd{"PEXPIRE", indexKey, ttlMs})
	ok, err := ix.fence.Exec(ctx, token, cmds)
	if err != nil {
		return fmt.Errorf("writing signature index: %w", err)
	}
	if !ok {
		ix.standDown()
		return ErrSuperseded
	}

	return ix.enforceCap(ctx, token)
}

// enforceCap evicts the lowest-last_seen signatures beyond maxSignatures
// (fence-guarded like every index write).
func (ix *Indexer) enforceCap(ctx context.Context, token int64) error {
	total, err := ix.rc.ZCard(ctx, indexKey).Result()
	if err != nil || total <= maxSignatures {
		return err
	}
	evict, err := ix.rc.ZRange(ctx, indexKey, 0, total-maxSignatures-1).Result()
	if err != nil {
		return err
	}
	cmds := make([]leasefence.Cmd, 0, 3*len(evict))
	for _, sig := range evict {
		cmds = append(cmds,
			leasefence.Cmd{"DEL", sigKey(sig), refsKey(sig)},
			leasefence.Cmd{"ZREM", indexKey, sig},
			leasefence.Cmd{"SREM", retrySigsKey, sig})
	}
	ok, err := ix.fence.Exec(ctx, token, cmds)
	if err != nil {
		return err
	}
	if !ok {
		ix.standDown()
		return ErrSuperseded
	}
	return nil
}

// ----------------------------------------------------------------------------
// Feeder 2: retry sampler
// ----------------------------------------------------------------------------

// retryAccum is one signature's per-sample observation.
type retryAccum struct {
	template string
	counts   map[[2]string]int64
	first    time.Time
	last     time.Time
	refs     []redis.Z
}

// SampleRetryNow runs one bounded resample of every queue's retry zset and
// replaces the store's retry-sampled contributions wholesale: contributions
// are the LAST observation, never an accumulation, because the same retrying
// task reappears in every sample. A signature whose retry tasks drained (and
// which has no archived observations) is removed entirely.
func (ix *Indexer) SampleRetryNow(ctx context.Context) error {
	now := time.Now()

	qnames, err := ix.rc.SMembers(ctx, allQueuesKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading queue set: %w", err)
	}
	sort.Strings(qnames)

	observed := make(map[string]*retryAccum)
	for _, q := range qnames {
		// A bounded window in score (next_process_at) order — deliberately
		// not cursor-tailed (§5.7; see the file header).
		ids, err := ix.rc.ZRange(ctx, retryKey(q), 0, int64(ix.cfg.MaxSamplePerQueue-1)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("sampling retry %q: %w", q, err)
		}
		for _, id := range ids {
			ti, gerr := ix.insp.GetTaskInfo(q, id)
			if gerr != nil {
				continue // dequeued between ZRANGE and fetch
			}
			if ti.State != asynq.TaskStateRetry || ti.LastErr == "" {
				continue
			}
			template := Normalize(ti.LastErr)
			if template == "" {
				continue
			}
			sig := SigHash(template)
			a := observed[sig]
			if a == nil {
				a = &retryAccum{template: template, counts: make(map[[2]string]int64)}
				observed[sig] = a
			}
			failedAt := ti.LastFailedAt
			if failedAt.IsZero() {
				failedAt = now
			}
			a.counts[[2]string{q, ti.Type}]++
			if a.first.IsZero() || failedAt.Before(a.first) {
				a.first = failedAt
			}
			if failedAt.After(a.last) {
				a.last = failedAt
			}
			if len(a.refs) < maxRefsPerSigPerRun {
				a.refs = append(a.refs, redis.Z{Score: float64(failedAt.Unix()), Member: TaskRef{Queue: q, ID: id}.member()})
			}
		}
	}

	// Union of previously-sampled signatures and this run's observations:
	// both must be rewritten (a drained signature's retry counts go to zero).
	prev, err := ix.rc.SMembers(ctx, retrySigsKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading retry-sampled set: %w", err)
	}
	union := make(map[string]bool, len(prev)+len(observed))
	for _, sig := range prev {
		union[sig] = true
	}
	for sig := range observed {
		union[sig] = true
	}
	sigsList := make([]string, 0, len(union))
	for sig := range union {
		sigsList = append(sigsList, sig)
	}
	sort.Strings(sigsList)

	existing, _, err := readSignatureHashes(ctx, ix.rc, sigsList)
	if err != nil {
		return fmt.Errorf("reading signature hashes: %w", err)
	}

	// Fence-guarded batch (§5.13, phase 12); token 0 = off-lease
	// SampleRetryNow, unfenced by design.
	token := ix.currentToken()
	ttlMs := indexTTL.Milliseconds()
	cmds := make([]leasefence.Cmd, 0, 6*len(sigsList)+6)
	var stillSampled []interface{}
	for _, sig := range sigsList {
		s := existing[sig]
		acc := observed[sig]
		if s == nil && acc == nil {
			continue
		}
		if s == nil {
			s = &Signature{Sig: sig, Template: acc.template, PrevCount: -1}
		}
		// Wholesale replace: zero every retry contribution, then apply this
		// run's observations.
		for i := range s.Counts {
			s.Counts[i].RetrySampled = 0
		}
		if acc != nil {
			for qt, n := range acc.counts {
				s.cell(qt[0], qt[1]).RetrySampled = n
			}
			if s.FirstSeen.IsZero() || (!acc.first.IsZero() && acc.first.Before(s.FirstSeen)) {
				s.FirstSeen = acc.first
			}
			if acc.last.After(s.LastSeen) {
				s.LastSeen = acc.last
			}
		}
		// Drop zeroed cells; a signature with no archived history and no
		// live retry contributions has drained — remove it entirely.
		kept := s.Counts[:0]
		for _, c := range s.Counts {
			if c.Archived > 0 || c.RetrySampled > 0 {
				kept = append(kept, c)
			}
		}
		s.Counts = kept
		if s.ArchivedCount == 0 && s.RetrySampledTotal() == 0 {
			cmds = append(cmds,
				leasefence.Cmd{"DEL", sigKey(sig), refsKey(sig)},
				leasefence.Cmd{"ZREM", indexKey, sig})
			continue
		}
		s.rollSnapshot(now)
		fields, herr := s.toHash()
		if herr != nil {
			return fmt.Errorf("encoding signature %s: %w", sig, herr)
		}
		cmds = append(cmds,
			hsetCmd(sigKey(sig), fields),
			leasefence.Cmd{"PEXPIRE", sigKey(sig), ttlMs},
			leasefence.Cmd{"ZADD", indexKey, float64(s.LastSeen.Unix()), sig})
		if acc != nil && len(acc.refs) > 0 {
			zadd := leasefence.Cmd{"ZADD", refsKey(sig)}
			for _, r := range acc.refs {
				zadd = append(zadd, r.Score, r.Member)
			}
			cmds = append(cmds, zadd,
				leasefence.Cmd{"ZREMRANGEBYRANK", refsKey(sig), 0, int64(-(maxRefsPerSig + 1))},
				leasefence.Cmd{"PEXPIRE", refsKey(sig), ttlMs})
		}
		if s.RetrySampledTotal() > 0 {
			stillSampled = append(stillSampled, sig)
		}
	}
	cmds = append(cmds, leasefence.Cmd{"DEL", retrySigsKey})
	if len(stillSampled) > 0 {
		sadd := leasefence.Cmd{"SADD", retrySigsKey}
		sadd = append(sadd, stillSampled...)
		cmds = append(cmds, sadd, leasefence.Cmd{"PEXPIRE", retrySigsKey, ttlMs})
	}
	cmds = append(cmds,
		leasefence.Cmd{"HSET", metaKey, "sample_at", strconv.FormatInt(now.Unix(), 10)},
		leasefence.Cmd{"PEXPIRE", metaKey, ttlMs},
		leasefence.Cmd{"PEXPIRE", indexKey, ttlMs})
	ok, err := ix.fence.Exec(ctx, token, cmds)
	if err != nil {
		return fmt.Errorf("writing retry sample: %w", err)
	}
	if !ok {
		ix.standDown()
		return ErrSuperseded
	}
	return nil
}
