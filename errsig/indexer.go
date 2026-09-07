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

	"github.com/zkrebbekx/asynqmon/internal/leasefence"
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

	// defaultTailSafetyLag keeps the archived-tail cursor out of the second
	// that is still running. asynq scores the archived zset in whole
	// seconds (died_at). The cursor is a (score, id) pair and the sweep
	// skips a tie it already served, so a task archived LATER in the same
	// second as the cursor, whose random UUID sorts below the cursor id,
	// would be skipped forever. The sweep therefore stops at
	// now - defaultTailSafetyLag; entries above that boundary wait for the
	// next sweep. Two seconds also absorb a small clock difference between
	// the workers that write died_at and the Redis TIME the sweep reads.
	defaultTailSafetyLag = 2 * time.Second

	// defaultCommandBudget is the indexer's Redis command spend cap in
	// commands per second. The stats engine's §5.1 governor covers only
	// stats.Engine; without a budget of its own the indexer issued one
	// unchunked N×3-command pipeline per sweep plus per-queue decode reads —
	// at tier-2/3 fleet sizes that alone could dwarf the governed sweep.
	// Small fleets are unaffected: a run that fits the budget behaves
	// exactly as before; beyond it the run rotates, resuming from a
	// holder-local cursor next tick, so every queue is still visited — just
	// across several ticks instead of one.
	defaultCommandBudget = 500

	// metaChunkSize bounds the per-queue metadata pipeline (ZCARD + GET +
	// HGET per queue): the old single pipeline hit 75k commands at 25k
	// queues — the classic big-pipeline blowup.
	metaChunkSize = 200

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
	// CommandBudget caps the indexer's Redis command spend, in commands per
	// second, governing tail sweeps and retry samples alike. Runs that
	// exceed it rotate across ticks from a holder-local cursor. Default 500.
	CommandBudget int
	// TailSafetyLag holds the archived-tail sweep back from the current
	// second, so no task can be archived into a second the cursor already
	// passed. Default 2s. Raise it when worker clocks differ from the Redis
	// clock by more than that.
	TailSafetyLag time.Duration
	// Now returns the current time. Default time.Now. Tests inject a clock
	// so they can step over TailSafetyLag instead of sleeping.
	Now func() time.Time
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

	// Budgeted-rotation state (holder-local; §5.7 command budget). tailRot /
	// sampleRot are the last queue each feeder finished, "" = start of the
	// sorted list; failedByQ / trimmedQ carry the last-known per-queue
	// failed-today counter and at-cap flag so the meta hash's fleet-wide
	// numbers stay complete across partial sweeps (values for queues not yet
	// visited this rotation are last-known — same honesty class as the stats
	// engine's carried-forward rows).
	tailRot   string
	sampleRot string
	failedByQ map[string]int64
	trimmedQ  map[string]bool
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
	if cfg.CommandBudget <= 0 {
		cfg.CommandBudget = defaultCommandBudget
	}
	if cfg.TailSafetyLag <= 0 {
		cfg.TailSafetyLag = defaultTailSafetyLag
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = defaultInstanceID()
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Indexer{
		cfg:       cfg,
		rc:        cfg.RedisClient,
		insp:      cfg.Inspector,
		logf:      logf,
		kick:      make(chan struct{}, 1),
		fence:     newIndexerFence(cfg.RedisClient),
		failedByQ: make(map[string]int64),
		trimmedQ:  make(map[string]bool),
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

// standDown clears holder state after the fenced write carrying `rejected`
// was refused, so the lease loop re-contends instead of retrying doomed
// writes. It skips the stand-down when `rejected` is older than the token
// this replica holds now (#52.1): the replica re-acquired the lease after
// its own lease expired mid-run, so the rejection is stale and the fresh
// lease must survive.
func (ix *Indexer) standDown(rejected int64) {
	leasefence.StandDown(&ix.token, &ix.holding, rejected)
}

// hsetCmd flattens a field map into one HSET command.
func hsetCmd(key string, fields map[string]interface{}) leasefence.Cmd {
	// No capacity hint: field maps are small and fixed-shape, and a
	// data-derived capacity is an overflow liability for no measurable gain.
	cmd := make(leasefence.Cmd, 0)
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

// rotateFrom returns qnames reordered to start just after `last` (circular),
// so a budget-limited run resumes where the previous one stopped and every
// queue is visited across ticks.
func rotateFrom(qnames []string, last string) []string {
	if len(qnames) == 0 || last == "" {
		return qnames
	}
	start := sort.SearchStrings(qnames, last)
	if start < len(qnames) && qnames[start] == last {
		start++
	}
	if start >= len(qnames) {
		return qnames
	}
	out := make([]string, 0, len(qnames))
	out = append(out, qnames[start:]...)
	out = append(out, qnames[:start]...)
	return out
}

// SweepTailNow runs one archived-tail sweep across every queue, regardless
// of lease ownership (exposed for tests and operational tooling; running it
// off-lease only costs duplicate reads and idempotent merges).
func (ix *Indexer) SweepTailNow(ctx context.Context) error {
	now := ix.cfg.Now()
	// Capture the fencing token ONCE, up front. A lease lost mid-sweep zeroes
	// ix.token, and reading it at write time would silently convert the
	// fenced merge into an unfenced one — the exact stale-holder overwrite
	// the fence exists to stop. With the captured token the CAS in
	// leasefence.Exec rejects a superseded holder instead. An explicitly
	// off-lease call (token 0 from the start — tests/tooling) keeps the
	// documented unfenced *Now semantics.
	token := ix.currentToken()

	qnames, err := ix.rc.SMembers(ctx, allQueuesKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading queue set: %w", err)
	}
	sort.Strings(qnames)
	// Queues deleted since the last rotation must not linger in the
	// holder-local meta accumulators.
	universe := make(map[string]bool, len(qnames))
	for _, q := range qnames {
		universe[q] = true
	}
	for q := range ix.failedByQ {
		if !universe[q] {
			delete(ix.failedByQ, q)
		}
	}
	for q := range ix.trimmedQ {
		if !universe[q] {
			delete(ix.trimmedQ, q)
		}
	}

	// Command budget for this sweep (§5.7 governor): a full pass that fits
	// spends exactly what it used to; beyond the budget the sweep stops and
	// the next tick resumes from the rotation cursor.
	budget := ix.cfg.CommandBudget * int(ix.cfg.TailInterval.Seconds())
	if budget < 4*metaChunkSize {
		budget = 4 * metaChunkSize // floor: always complete at least one chunk
	}
	spent := 1 // SMEMBERS

	// Highest died-at second this sweep may examine (see
	// defaultTailSafetyLag). Entries at or above the boundary stay for the
	// next sweep, so the cursor never enters a second that can still
	// receive entries.
	maxScore := now.Add(-ix.cfg.TailSafetyLag).Unix()

	agg := make(map[string]*tailAccum)
	newCursors := make(map[string]string)
	ordered := rotateFrom(qnames, ix.tailRot)
	completed := 0

	type qMeta struct {
		card   *redis.IntCmd
		failed *redis.StringCmd
		cursor *redis.StringCmd
	}
	for chunkStart := 0; chunkStart < len(ordered) && spent < budget; chunkStart += metaChunkSize {
		chunkEnd := chunkStart + metaChunkSize
		if chunkEnd > len(ordered) {
			chunkEnd = len(ordered)
		}
		chunk := ordered[chunkStart:chunkEnd]

		// Per-queue metadata, chunk-pipelined: archive size (trim
		// detection), today's failed counter (magnitude cross-check),
		// stored cursor.
		pipe := ix.rc.Pipeline()
		metas := make([]qMeta, len(chunk))
		for i, q := range chunk {
			metas[i] = qMeta{
				card:   pipe.ZCard(ctx, archivedKey(q)),
				failed: pipe.Get(ctx, FailedTodayKey(q, now)),
				cursor: pipe.HGet(ctx, cursorsKey, q),
			}
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("reading queue metadata: %w", err)
		}
		spent += 3 * len(chunk)

		for i, q := range chunk {
			if card, err := metas[i].card.Result(); err == nil {
				ix.trimmedQ[q] = card >= ArchiveTrimCap
			}
			if v, err := metas[i].failed.Result(); err == nil {
				ix.failedByQ[q] = decodeInt(v)
			} else {
				ix.failedByQ[q] = 0 // counter absent = no failures today
			}
			cur := tailCursor{}
			if v, err := metas[i].cursor.Result(); err == nil {
				cur = decodeCursor(v)
			}
			newCur, tailSpent, err := ix.tailQueue(ctx, q, cur, maxScore, agg)
			spent += tailSpent
			if err != nil {
				return fmt.Errorf("tailing archived %q: %w", q, err)
			}
			if newCur.valid && newCur != cur {
				newCursors[q] = newCur.encode()
			}
			ix.tailRot = q
			completed++
			if spent >= budget {
				break
			}
		}
	}
	if completed >= len(ordered) {
		ix.tailRot = "" // full rotation finished — start over next tick
	}

	// Fleet-wide meta numbers merge the holder-local accumulators so partial
	// sweeps never shrink them to the swept subset.
	var failedTotal int64
	for _, n := range ix.failedByQ {
		failedTotal += n
	}
	trimmed := make([]string, 0, len(ix.trimmedQ))
	for q, at := range ix.trimmedQ {
		if at {
			trimmed = append(trimmed, q)
		}
	}
	sort.Strings(trimmed)

	if err := ix.mergeAndWrite(ctx, now, token, agg, failedTotal, trimmed, newCursors); err != nil {
		return err
	}
	return nil
}

// tailQueue walks one queue's archived zset from its cursor up to maxScore,
// died-at ascending, decoding up to MaxTailPerQueue entries. Every entry examined —
// including ones whose task vanished (trim/delete) or moved state between
// the ZRANGE and the fetch — advances the cursor, so a poisoned or trimmed
// range can never wedge the tail.
func (ix *Indexer) tailQueue(ctx context.Context, q string, cur tailCursor, maxScore int64, agg map[string]*tailAccum) (tailCursor, int, error) {
	// Paging strategy: advance the score window (Min) to the ROLLING cursor
	// after every page, with an offset used only to step through runs of
	// equal scores. A fixed Min with a globally growing offset was not
	// trim-safe: asynq's archive trim removes the lowest-scored members, and
	// every removal between two pages shifted the offset window left — so
	// entries were skipped and lost forever behind the advanced cursor,
	// precisely on at-cap queues where trimming is constant. With Min pinned
	// to the cursor's score, trims strictly below the cursor cannot shift
	// the window at all; the residual exposure is one tie run at the trim
	// boundary, further guarded by the id filter below.
	minStr := "-inf"
	if cur.valid {
		minStr = strconv.FormatInt(cur.score, 10) // inclusive; ties filtered below
	}
	// The window stops at maxScore (inclusive), never at +inf: a second that
	// is still running can still receive entries, and the tie filter below
	// would skip every one of them whose id sorts under the cursor id.
	maxStr := strconv.FormatInt(maxScore, 10)
	if cur.valid && cur.score > maxScore {
		return cur, 0, nil // cursor already past the safe boundary
	}
	newCur := cur
	examined := 0
	spent := 0
	var offset int64

	for examined < ix.cfg.MaxTailPerQueue {
		entries, err := ix.rc.ZRangeByScoreWithScores(ctx, archivedKey(q), &redis.ZRangeBy{
			Min: minStr, Max: maxStr, Offset: offset, Count: tailBatchSize,
		}).Result()
		spent++
		if err != nil && !errors.Is(err, redis.Nil) {
			return newCur, spent, err
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
			if newCur.valid && (score < newCur.score || (score == newCur.score && id <= newCur.id)) {
				continue // tie already served (this sweep or a previous one)
			}
			examined++
			newCur = tailCursor{score: score, id: id, valid: true}

			spent++ // GetTaskInfo
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
		if len(entries) < tailBatchSize {
			break
		}
		// Re-anchor the window on the rolling cursor. Entries are
		// score-ordered, so the ones sharing the cursor's score are a
		// suffix of this page — that suffix size is the offset into the
		// next page's tie run. If the whole page was ties at the cursor
		// score (offset unchanged would refetch it forever), extend the
		// offset instead; either way each page strictly advances.
		if newCur.valid {
			anchored := strconv.FormatInt(newCur.score, 10)
			var sameScore int64
			for i := len(entries) - 1; i >= 0; i-- {
				if int64(entries[i].Score) != newCur.score {
					break
				}
				sameScore++
			}
			if anchored == minStr && sameScore == int64(len(entries)) {
				offset += int64(len(entries))
			} else {
				minStr = anchored
				offset = sameScore
			}
		} else {
			offset += int64(len(entries))
		}
	}
	return newCur, spent, nil
}

// mergeAndWrite folds one tail sweep's accumulations into the store: it
// loads EVERY tracked signature (bounded by maxSignatures), applies the new
// archived observations, rolls every signature's trend snapshot (untouched
// signatures must still roll toward a flat delta, or a dead signature would
// read "growing" forever), and writes back only what changed.
func (ix *Indexer) mergeAndWrite(ctx context.Context, now time.Time, token int64, agg map[string]*tailAccum, failedTotal int64, trimmed []string, newCursors map[string]string) error {
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
	// must never land over a newer holder's index — the token was captured
	// at sweep start (see SweepTailNow). Token 0 (explicitly off-lease
	// SweepTailNow — tests/tooling) writes unfenced by design.
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
		ix.standDown(token)
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
		ix.standDown(token)
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
	now := ix.cfg.Now()
	// Captured up front for the same reason as SweepTailNow: a lease lost
	// mid-run must fail the fence CAS, not silently write unfenced.
	token := ix.currentToken()

	qnames, err := ix.rc.SMembers(ctx, allQueuesKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("reading queue set: %w", err)
	}
	sort.Strings(qnames)

	// Command budget (§5.7 governor): the sampler used to walk EVERY queue
	// per run (1 ZRANGE + up to MaxSamplePerQueue GetTaskInfo each) with no
	// cap. Beyond the budget the run stops and the next one resumes from
	// the rotation cursor; the wholesale-replace below is scoped to the
	// queues actually swept so unswept queues' contributions survive.
	budget := ix.cfg.CommandBudget * int(ix.cfg.SampleInterval.Seconds())
	if budget < 2*ix.cfg.MaxSamplePerQueue {
		budget = 2 * ix.cfg.MaxSamplePerQueue // floor: always finish one queue
	}
	spent := 1 // SMEMBERS
	ordered := rotateFrom(qnames, ix.sampleRot)
	swept := make(map[string]bool, len(ordered))
	completed := 0

	observed := make(map[string]*retryAccum)
	for _, q := range ordered {
		if spent >= budget {
			break
		}
		// A bounded window in score (next_process_at) order — deliberately
		// not cursor-tailed (§5.7; see the file header).
		ids, err := ix.rc.ZRange(ctx, retryKey(q), 0, int64(ix.cfg.MaxSamplePerQueue-1)).Result()
		spent++
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("sampling retry %q: %w", q, err)
		}
		for _, id := range ids {
			spent++ // GetTaskInfo
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
		swept[q] = true
		ix.sampleRot = q
		completed++
	}
	if completed >= len(ordered) {
		ix.sampleRot = "" // full rotation finished
	}

	// Union of previously-sampled signatures and this run's observations:
	// both must be rewritten (a drained signature's retry counts go to zero
	// — but only for cells in queues this run actually swept; a budgeted
	// partial run must not zero contributions it never re-observed).
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

	// Fence-guarded batch (§5.13, phase 12); token captured at run start;
	// token 0 = explicitly off-lease SampleRetryNow, unfenced by design.
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
		// Wholesale replace, scoped to the swept queues: zero their retry
		// contributions, then apply this run's observations. Cells in
		// unswept queues keep their last observation until their rotation
		// turn.
		for i := range s.Counts {
			if swept[s.Counts[i].Queue] {
				s.Counts[i].RetrySampled = 0
			}
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
		ix.standDown(token)
		return ErrSuperseded
	}
	return nil
}
