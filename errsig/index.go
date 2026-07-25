package errsig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - the Redis layout of the signature store (all under asynqmon:idx:err*)
//   - Signature / QueueTypeCount / TaskRef and their hash codec
//   - the engine-independent read path (ReadIndex, ReadRefs) any replica —
//     and the HTTP handlers — can serve from without running the indexer
//   - the asynq key formats the feeders read, mirrored locally (same
//     version-pinned rationale as stats/keys.go and aql_exec.go: pinned to
//     asynq v0.24.1, read-only, misses degrade to "unknown")
// ****************************************************************************

const (
	// indexKey is the ZSET of signature hashes scored by last_seen (Unix
	// seconds) — the index the list endpoint reads.
	indexKey = "asynqmon:idx:err"

	// sigKeyPrefix + <sighash> is the HASH holding one signature record;
	// + ":refs" is the capped ZSET of sample task refs ("queue/id" scored by
	// observed-at). Sighashes are 32 hex chars, so the fixed-name keys below
	// can never collide with a signature key.
	sigKeyPrefix = "asynqmon:idx:err:"

	// cursorsKey is the HASH of per-queue archived-tail cursors
	// (field = queue, value = "<died-at-unix>|<task-id>").
	cursorsKey = "asynqmon:idx:err:cursors"

	// metaKey is the HASH of index-wide metadata: failed_today_total (the
	// §5.7 magnitude cross-check), trimmed_queues JSON, tail_at, sample_at.
	metaKey = "asynqmon:idx:err:meta"

	// retrySigsKey is the SET of sighashes currently carrying retry-sampled
	// contributions — the sampler's replace-wholesale bookkeeping.
	retrySigsKey = "asynqmon:idx:err:retrysigs"

	// indexTTL mirrors the archive's own 90-day bound (§5.7 "TTL mirroring
	// the archive"): every write refreshes it, so the store dies with its
	// source data if the indexer stops.
	indexTTL = 90 * 24 * time.Hour

	// maxRefsPerSig caps each signature's sample-ref zset (newest kept).
	maxRefsPerSig = 100

	// maxSignatures caps the index population; beyond it the lowest-last_seen
	// signatures are evicted. Bounds both storage and the per-sweep full-load
	// (the trend snapshot roll reads every signature each tail sweep).
	maxSignatures = 1000

	// ArchiveTrimCap is asynq's per-queue archive bound (§7.3): a queue whose
	// archived ZCARD sits at this value is discarding forensic data, and every
	// count sourced from it is a lower bound.
	ArchiveTrimCap = 10000
)

func sigKey(sig string) string  { return sigKeyPrefix + sig }
func refsKey(sig string) string { return sigKeyPrefix + sig + ":refs" }

// ----------------------------------------------------------------------------
// asynq's internal key formats, mirrored locally (read-only; see file header).
// ----------------------------------------------------------------------------

const allQueuesKey = "asynq:queues"

func queueKeyPrefix(qname string) string { return fmt.Sprintf("asynq:{%s}:", qname) }
func archivedKey(qname string) string    { return queueKeyPrefix(qname) + "archived" }
func retryKey(qname string) string       { return queueKeyPrefix(qname) + "retry" }

// FailedTodayKey returns the queue's daily failed counter key for t's UTC
// date. Exported for the detail handler's live per-queue counter cross-check.
func FailedTodayKey(qname string, t time.Time) string {
	return fmt.Sprintf("%sfailed:%s", queueKeyPrefix(qname), t.UTC().Format("2006-01-02"))
}

// ----------------------------------------------------------------------------
// Model
// ----------------------------------------------------------------------------

// QueueTypeCount is one (queue, task type) cell of a signature's blast
// radius. Archived counts are monotonic accumulations from the exact tail;
// RetrySampled is the LAST retry resample's observation (replaced wholesale
// every sampler run, never accumulated — resampling the same task twice must
// not double-count).
type QueueTypeCount struct {
	Queue        string `json:"queue"`
	Type         string `json:"type"`
	Archived     int64  `json:"archived"`
	RetrySampled int64  `json:"retry_sampled"`
}

// TaskRef points at one sample task ("find me a real instance of this
// failure"). The peek wire format is "<queue>/<id>"; task IDs are UUIDs and
// never contain "/", so the LAST slash splits even slash-bearing queue names
// (same rule as the console's peek param).
type TaskRef struct {
	Queue string `json:"queue"`
	ID    string `json:"id"`
}

func (r TaskRef) member() string { return r.Queue + "/" + r.ID }

func refFromMember(m string) (TaskRef, bool) {
	i := strings.LastIndex(m, "/")
	if i <= 0 || i == len(m)-1 {
		return TaskRef{}, false
	}
	return TaskRef{Queue: m[:i], ID: m[i+1:]}, true
}

// Signature is one tracked failure signature.
type Signature struct {
	Sig      string
	Template string

	// FirstSeen/LastSeen are observation bounds: died-at scores from the
	// archived tail, LastFailedAt from the retry sampler.
	FirstSeen time.Time
	LastSeen  time.Time

	// ArchivedCount is the exact, monotonic count of archived observations.
	ArchivedCount int64

	// Counts is the queue×type matrix (see QueueTypeCount).
	Counts []QueueTypeCount

	// Trend snapshot state (trend.go). PrevCount is -1 until one full
	// window has completed.
	SnapCount int64
	PrevCount int64
	SnapAt    time.Time
}

// RetrySampledTotal sums the current retry-resample contributions.
func (s *Signature) RetrySampledTotal() int64 {
	var n int64
	for _, c := range s.Counts {
		n += c.RetrySampled
	}
	return n
}

// TotalCount is the signature's headline count: exact archived observations
// plus the current retry resample.
func (s *Signature) TotalCount() int64 { return s.ArchivedCount + s.RetrySampledTotal() }

// Queues returns the distinct source queues, sorted.
func (s *Signature) Queues() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range s.Counts {
		if !seen[c.Queue] {
			seen[c.Queue] = true
			out = append(out, c.Queue)
		}
	}
	sort.Strings(out)
	return out
}

// Sampled reports whether any contribution came from the retry sampler.
func (s *Signature) Sampled() bool { return s.RetrySampledTotal() > 0 }

// sortCounts keeps the matrix deterministic (queue, then type).
func (s *Signature) sortCounts() {
	sort.Slice(s.Counts, func(i, j int) bool {
		if s.Counts[i].Queue != s.Counts[j].Queue {
			return s.Counts[i].Queue < s.Counts[j].Queue
		}
		return s.Counts[i].Type < s.Counts[j].Type
	})
}

// cell returns the matrix cell for (queue, type), creating it if absent.
func (s *Signature) cell(queue, typ string) *QueueTypeCount {
	for i := range s.Counts {
		if s.Counts[i].Queue == queue && s.Counts[i].Type == typ {
			return &s.Counts[i]
		}
	}
	s.Counts = append(s.Counts, QueueTypeCount{Queue: queue, Type: typ})
	return &s.Counts[len(s.Counts)-1]
}

// ----------------------------------------------------------------------------
// Hash codec (mirrors the scheduler-snapshot pattern: JSON for the matrix,
// unix seconds for times, decode misses degrade to absent-not-fabricated).
// ----------------------------------------------------------------------------

func encodeTime(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func decodeTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func decodeInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (s *Signature) toHash() (map[string]interface{}, error) {
	s.sortCounts()
	countsJSON, err := json.Marshal(s.Counts)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"template":       s.Template,
		"first_seen":     encodeTime(s.FirstSeen),
		"last_seen":      encodeTime(s.LastSeen),
		"archived_count": strconv.FormatInt(s.ArchivedCount, 10),
		"counts":         string(countsJSON),
		"snap_count":     strconv.FormatInt(s.SnapCount, 10),
		"prev_count":     strconv.FormatInt(s.PrevCount, 10),
		"snap_at":        encodeTime(s.SnapAt),
	}, nil
}

// signatureFromHash decodes a signature hash; nil when the template is
// missing (expired mid-read, or written by a future version).
func signatureFromHash(sig string, h map[string]string) *Signature {
	if h["template"] == "" {
		return nil
	}
	s := &Signature{
		Sig:           sig,
		Template:      h["template"],
		FirstSeen:     decodeTime(h["first_seen"]),
		LastSeen:      decodeTime(h["last_seen"]),
		ArchivedCount: decodeInt(h["archived_count"]),
		SnapCount:     decodeInt(h["snap_count"]),
		PrevCount:     -1,
		SnapAt:        decodeTime(h["snap_at"]),
	}
	if v, ok := h["prev_count"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			s.PrevCount = n
		}
	}
	_ = json.Unmarshal([]byte(h["counts"]), &s.Counts) // optional; empty matrix degrades honestly
	return s
}

// ----------------------------------------------------------------------------
// Read path (engine-independent)
// ----------------------------------------------------------------------------

// Meta is the index-wide metadata the honesty ribbon and the magnitude
// cross-check render from.
type Meta struct {
	// FailedTodayTotal is the fleet-wide sum of today's failed counters at
	// the last tail sweep — counter-derived magnitude, never trimmed.
	FailedTodayTotal int64
	// TrimmedQueues are queues whose archived zset sat at the 10k trim cap
	// at the last tail sweep; counts sourced from them are lower bounds.
	TrimmedQueues []string
	// TailAt / SampleAt stamp the feeders' last completed runs (zero = never).
	TailAt   time.Time
	SampleAt time.Time
}

// ReadIndex returns every tracked signature (unsorted — handlers own sort/
// limit) plus the index metadata. Plain Redis reads, so any replica serves
// it; before any indexer run the result is simply empty, never an error.
func ReadIndex(ctx context.Context, rc redis.UniversalClient) ([]*Signature, *Meta, error) {
	sigs, err := rc.ZRange(ctx, indexKey, 0, -1).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil, err
	}
	bySig, _, err := readSignatureHashes(ctx, rc, sigs)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*Signature, 0, len(bySig))
	for _, sig := range sigs {
		if s, ok := bySig[sig]; ok {
			out = append(out, s)
		}
	}
	meta, err := readMeta(ctx, rc)
	if err != nil {
		return nil, nil, err
	}
	return out, meta, nil
}

// ReadSignature returns one signature, or nil when unknown.
func ReadSignature(ctx context.Context, rc redis.UniversalClient, sig string) (*Signature, error) {
	h, err := rc.HGetAll(ctx, sigKey(sig)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if len(h) == 0 {
		return nil, nil
	}
	return signatureFromHash(sig, h), nil
}

// ReadRefs returns up to limit sample task refs for the signature, newest
// observation first.
func ReadRefs(ctx context.Context, rc redis.UniversalClient, sig string, limit int) ([]TaskRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	members, err := rc.ZRevRange(ctx, refsKey(sig), 0, int64(limit-1)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]TaskRef, 0, len(members))
	for _, m := range members {
		if r, ok := refFromMember(m); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// ReadMeta returns the index-wide metadata alone (the detail handler's
// cheap path — no full index load).
func ReadMeta(ctx context.Context, rc redis.UniversalClient) (*Meta, error) {
	return readMeta(ctx, rc)
}

func readMeta(ctx context.Context, rc redis.UniversalClient) (*Meta, error) {
	h, err := rc.HGetAll(ctx, metaKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	m := &Meta{
		FailedTodayTotal: decodeInt(h["failed_today_total"]),
		TailAt:           decodeTime(h["tail_at"]),
		SampleAt:         decodeTime(h["sample_at"]),
	}
	if h["trimmed_queues"] != "" {
		_ = json.Unmarshal([]byte(h["trimmed_queues"]), &m.TrimmedQueues)
	}
	return m, nil
}

// readSignatureHashes pipelines HGETALL for the given sighashes, skipping
// missing/unparsable ones. Returns the command count for sweep budgeting.
func readSignatureHashes(ctx context.Context, rc redis.UniversalClient, sigs []string) (map[string]*Signature, int, error) {
	if len(sigs) == 0 {
		return map[string]*Signature{}, 0, nil
	}
	pipe := rc.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(sigs))
	for i, sig := range sigs {
		cmds[i] = pipe.HGetAll(ctx, sigKey(sig))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, len(sigs), err
	}
	out := make(map[string]*Signature, len(sigs))
	for i, sig := range sigs {
		h, err := cmds[i].Result()
		if err != nil || len(h) == 0 {
			continue
		}
		if s := signatureFromHash(sig, h); s != nil {
			out[sig] = s
		}
	}
	return out, len(sigs), nil
}
