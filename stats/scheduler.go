package stats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - scheduler entry snapshots (build contract §3.8/§5.12): every entry the
//     sweeper observes is persisted under a STABLE key — hash(spec|type|
//     payload|opts) — so history survives scheduler restarts (asynq's entry
//     IDs are fresh UUIDs per process) and a dead scheduler leaves a visible
//     corpse instead of silently vanishing from the UI
//   - the live-vs-snapshot diff that yields SCHEDULER GONE observations for
//     the attention engine (detector #9) and the /api/schedulers merge
//   - the read path (ReadSchedulerSnapshots), engine-independent so any
//     replica — even one with the stats sweeper disabled — can serve the
//     snapshot rows
// ****************************************************************************

const (
	// DefaultSchedulerGoneAfter is how stale a snapshot's last_seen must be —
	// with no live counterpart — before the entry is declared GONE. Two
	// sweeper heartbeats (the 15s lease TTL) of silence: long enough that a
	// scheduler restart or a missed asynq heartbeat (5s cadence) does not
	// flap, short enough that a dead scheduler is a finding within a minute.
	DefaultSchedulerGoneAfter = 30 * time.Second

	// schedulerSnapshotHorizon bounds snapshot retention: an entry not
	// observed for this long is pruned from the index and its hash deleted,
	// so unregistered-forever entries do not accumulate without bound.
	schedulerSnapshotHorizon = 30 * 24 * time.Hour

	// maxSchedulerEntryIDs caps the per-snapshot list of observed ephemeral
	// entry IDs. Each scheduler restart mints a new ID; keeping the recent
	// ones lets the outcomes endpoint merge enqueue history across restarts
	// (the §5.12 "re-keyed to the stable hash" indirection).
	maxSchedulerEntryIDs = 8

	// DefaultTargetQueue is where asynq enqueues a task when no Queue option
	// is present on the entry.
	DefaultTargetQueue = "default"
)

// SchedulerEntryData is the observed shape of one scheduler entry, stored as
// JSON in the snapshot hash. Payload is the raw task payload (base64 in
// JSON); formatting for display happens at the API edge, where the
// PayloadFormatter lives.
type SchedulerEntryData struct {
	EntryID      string    `json:"entry_id"` // ephemeral ID at last observation
	Spec         string    `json:"spec"`
	TaskType     string    `json:"task_type"`
	Payload      []byte    `json:"payload,omitempty"`
	Opts         []string  `json:"opts,omitempty"`
	Queue        string    `json:"queue"`         // target queue (Queue opt, default "default")
	RetentionSec int64     `json:"retention_sec"` // Retention opt in whole seconds; 0 = none
	Next         time.Time `json:"next"`
	Prev         time.Time `json:"prev"`
}

// SchedulerSnapshot is one persisted scheduler entry observation.
type SchedulerSnapshot struct {
	StableKey string
	Entry     SchedulerEntryData
	// EntryIDs are the recently observed ephemeral IDs, newest first (capped
	// at maxSchedulerEntryIDs). Enqueue-event history lives under these in
	// asynq's own keys.
	EntryIDs  []string
	FirstSeen time.Time
	LastSeen  time.Time
}

// SchedulerGoneObs is one GONE determination handed to the attention engine:
// a snapshot whose last_seen is older than the gone threshold with no live
// counterpart. The state is time-gated by construction — no debounce needed.
type SchedulerGoneObs struct {
	StableKey string
	TaskType  string
	Queue     string // target queue, for the finding's queue column
	LastSeen  time.Time
}

// StableSchedulerKey derives the stable identity of a scheduler entry:
// sha256 over spec, task type, raw payload and the normalized (sorted)
// option strings. Ephemeral entry IDs never participate, so the key survives
// scheduler restarts; any change to what the entry does (spec, type,
// payload, options) is honestly a different entry.
func StableSchedulerKey(spec, taskType string, payload []byte, opts []string) string {
	sorted := make([]string, len(opts))
	copy(sorted, opts)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(spec))
	h.Write([]byte{0})
	h.Write([]byte(taskType))
	h.Write([]byte{0})
	h.Write(payload)
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(sorted, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))
}

// SchedulerEntryDataFromAsynq extracts the observed shape from a live asynq
// entry, resolving the target queue and retention from the typed option
// values (parseOption already ran inside the Inspector).
func SchedulerEntryDataFromAsynq(e *asynq.SchedulerEntry) SchedulerEntryData {
	data := SchedulerEntryData{
		EntryID:  e.ID,
		Spec:     e.Spec,
		TaskType: e.Task.Type(),
		Payload:  e.Task.Payload(),
		Queue:    DefaultTargetQueue,
		Next:     e.Next,
		Prev:     e.Prev,
	}
	for _, o := range e.Opts {
		data.Opts = append(data.Opts, o.String())
		switch o.Type() {
		case asynq.QueueOpt:
			if q, ok := o.Value().(string); ok && q != "" {
				data.Queue = q
			}
		case asynq.RetentionOpt:
			if d, ok := o.Value().(time.Duration); ok {
				data.RetentionSec = int64(d / time.Second)
			}
		}
	}
	return data
}

// StableKeyForAsynqEntry is StableSchedulerKey over a live entry's fields.
func StableKeyForAsynqEntry(e *asynq.SchedulerEntry) string {
	opts := make([]string, 0, len(e.Opts))
	for _, o := range e.Opts {
		opts = append(opts, o.String())
	}
	return StableSchedulerKey(e.Spec, e.Task.Type(), e.Task.Payload(), opts)
}

// ----------------------------------------------------------------------------
// Sweep integration.
// ----------------------------------------------------------------------------

// sweepSchedulers performs the per-sweep §5.12 pass: read the snapshot set,
// upsert every live entry under its stable key, and diff for GONE
// observations. Snapshots past the retention horizon are pruned. Returns the
// GONE list plus the Redis command counts (SchedulerEntries goes through the
// Inspector's own client and is not counted, matching how Servers() is
// treated in the sweep budget).
func (e *Engine) sweepSchedulers(ctx context.Context, now time.Time) (gone []SchedulerGoneObs, reads, writes int, err error) {
	live, err := e.insp.SchedulerEntries()
	if err != nil {
		return nil, reads, writes, err
	}

	keys, err := e.rc.ZRange(ctx, schedIndexKey, 0, -1).Result()
	reads++
	if err != nil {
		return nil, reads, writes, err
	}
	existing, n, err := readSchedulerHashes(ctx, e.rc, keys)
	reads += n
	if err != nil {
		return nil, reads, writes, err
	}

	liveByKey := make(map[string]*asynq.SchedulerEntry, len(live))
	for _, le := range live {
		liveByKey[StableKeyForAsynqEntry(le)] = le
	}

	pipe := e.rc.Pipeline()
	for key, le := range liveByKey {
		data := SchedulerEntryDataFromAsynq(le)
		firstSeen := now
		var entryIDs []string
		if old, ok := existing[key]; ok {
			if !old.FirstSeen.IsZero() {
				firstSeen = old.FirstSeen
			}
			entryIDs = old.EntryIDs
		}
		entryIDs = pushEntryID(entryIDs, le.ID)
		snap := &SchedulerSnapshot{
			StableKey: key,
			Entry:     data,
			EntryIDs:  entryIDs,
			FirstSeen: firstSeen,
			LastSeen:  now,
		}
		fields, err := snap.toHash()
		if err != nil {
			return nil, reads, writes, err
		}
		pipe.HSet(ctx, schedSnapshotKey(key), fields)
		pipe.ZAdd(ctx, schedIndexKey, redis.Z{Score: float64(now.Unix()), Member: key})
		writes += 2
	}

	goneAfter := e.cfg.SchedulerGoneAfter
	for key, snap := range existing {
		if _, isLive := liveByKey[key]; isLive {
			continue
		}
		age := now.Sub(snap.LastSeen)
		switch {
		case age > schedulerSnapshotHorizon:
			pipe.Del(ctx, schedSnapshotKey(key))
			pipe.ZRem(ctx, schedIndexKey, key)
			writes += 2
		case age > goneAfter:
			gone = append(gone, SchedulerGoneObs{
				StableKey: key,
				TaskType:  snap.Entry.TaskType,
				Queue:     snap.Entry.Queue,
				LastSeen:  snap.LastSeen,
			})
		}
	}
	if writes > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, reads, writes, err
		}
	}

	// Stable order for the attention report (map iteration is random).
	sort.Slice(gone, func(i, j int) bool {
		if gone[i].TaskType != gone[j].TaskType {
			return gone[i].TaskType < gone[j].TaskType
		}
		return gone[i].StableKey < gone[j].StableKey
	})
	return gone, reads, writes, nil
}

// pushEntryID prepends id to ids (dedup, newest first, capped).
func pushEntryID(ids []string, id string) []string {
	if id == "" {
		return ids
	}
	out := make([]string, 0, len(ids)+1)
	out = append(out, id)
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	if len(out) > maxSchedulerEntryIDs {
		out = out[:maxSchedulerEntryIDs]
	}
	return out
}

// ----------------------------------------------------------------------------
// Read path + hash encoding.
// ----------------------------------------------------------------------------

// ReadSchedulerSnapshots returns every persisted scheduler snapshot, most
// recently seen first. Engine-independent (plain Redis reads) so replicas
// with the sweeper disabled — and the API handlers — can serve rows; when no
// sweeper has ever run the result is simply empty, never an error.
func ReadSchedulerSnapshots(ctx context.Context, rc redis.UniversalClient) ([]*SchedulerSnapshot, error) {
	keys, err := rc.ZRevRange(ctx, schedIndexKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	byKey, _, err := readSchedulerHashes(ctx, rc, keys)
	if err != nil {
		return nil, err
	}
	out := make([]*SchedulerSnapshot, 0, len(byKey))
	for _, k := range keys { // preserve zset (recency) order
		if s, ok := byKey[k]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// readSchedulerHashes pipelines HGETALL for the given stable keys, skipping
// hashes that are missing or unparsable (pruned mid-read, or written by a
// future version): better a missing row than a fabricated one. Returns the
// number of Redis commands issued.
func readSchedulerHashes(ctx context.Context, rc redis.UniversalClient, keys []string) (map[string]*SchedulerSnapshot, int, error) {
	if len(keys) == 0 {
		return map[string]*SchedulerSnapshot{}, 0, nil
	}
	pipe := rc.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.HGetAll(ctx, schedSnapshotKey(k))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, len(keys), err
	}
	out := make(map[string]*SchedulerSnapshot, len(keys))
	for i, k := range keys {
		h, err := cmds[i].Result()
		if err != nil || len(h) == 0 {
			continue
		}
		if s := schedulerSnapshotFromHash(k, h); s != nil {
			out[k] = s
		}
	}
	return out, len(keys), nil
}

// toHash encodes the snapshot as Redis hash fields (§5.12 layout: entry
// JSON + first_seen/last_seen/last_entry_id, plus the entry-ID history).
func (s *SchedulerSnapshot) toHash() (map[string]interface{}, error) {
	entryJSON, err := json.Marshal(s.Entry)
	if err != nil {
		return nil, err
	}
	idsJSON, err := json.Marshal(s.EntryIDs)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"entry":         string(entryJSON),
		"first_seen":    encodeTime(s.FirstSeen),
		"last_seen":     encodeTime(s.LastSeen),
		"last_entry_id": s.Entry.EntryID,
		"entry_ids":     string(idsJSON),
	}, nil
}

// schedulerSnapshotFromHash decodes a snapshot hash; nil when the entry JSON
// is missing or unparsable (treat as absent, never fabricate).
func schedulerSnapshotFromHash(stableKey string, h map[string]string) *SchedulerSnapshot {
	var entry SchedulerEntryData
	if err := json.Unmarshal([]byte(h["entry"]), &entry); err != nil {
		return nil
	}
	var ids []string
	_ = json.Unmarshal([]byte(h["entry_ids"]), &ids) // optional; degrade to last_entry_id
	if len(ids) == 0 && h["last_entry_id"] != "" {
		ids = []string{h["last_entry_id"]}
	}
	return &SchedulerSnapshot{
		StableKey: stableKey,
		Entry:     entry,
		EntryIDs:  ids,
		FirstSeen: decodeTime(h["first_seen"]),
		LastSeen:  decodeTime(h["last_seen"]),
	}
}
