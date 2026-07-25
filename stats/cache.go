package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/internal/leasefence"
)

// ****************************************************************************
// This file defines:
//   - the in-process snapshot store (zero-latency reads on the lease holder)
//   - reading and writing the shared Redis cache (reads for standby replicas;
//     the write path is fence-guarded on the leased sweep, §5.13 phase 12)
// ****************************************************************************

// memoryStore holds the latest sweep results in process. Snapshots are
// treated as immutable once published: the sweeper always builds fresh
// structs and replaces the whole map, so readers may hold returned pointers
// across the RWMutex boundary without copies.
type memoryStore struct {
	mu        sync.RWMutex
	fleet     *FleetSnapshot
	queues    map[string]*QueueSnapshot
	attention *AttentionReport
	lastSweep SweepStats
}

// SweepStats records the measured cost of the most recent sweep — the
// phase-12 command-budget governor's own accounting and the Settings › Health
// readout (§3.12).
type SweepStats struct {
	At time.Time
	// Queues is the fleet size (SCARD asynq:queues); Refreshed is how many
	// queues this tick actually re-read (equal in tier 1 / full mode).
	Queues    int
	Refreshed int
	ReadCmds  int // Redis commands issued to read asynq state
	WriteCmds int // Redis commands issued to write the asynqmon cache
	Duration  time.Duration
	// Governor state of the sweep that produced these numbers (§5.1).
	Tier               int
	HotSetSize         int
	FullRotationEstSec int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{}
}

func (m *memoryStore) replace(fleet *FleetSnapshot, queues map[string]*QueueSnapshot, attention *AttentionReport, stats SweepStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fleet = fleet
	m.queues = queues
	m.attention = attention
	m.lastSweep = stats
}

// get returns the fleet snapshot and all queue snapshots (sorted by name for
// a stable baseline order). Returns (nil, nil) when no sweep has run here.
func (m *memoryStore) get() (*FleetSnapshot, []*QueueSnapshot) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.fleet == nil {
		return nil, nil
	}
	out := make([]*QueueSnapshot, 0, len(m.queues))
	for _, s := range m.queues {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Queue < out[j].Queue })
	return m.fleet, out
}

func (m *memoryStore) sweepStats() SweepStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastSweep
}

// getAttention returns the locally-swept attention report plus the fleet
// snapshot it was computed from (whose RefreshedAt decides freshness).
// (nil, nil) when this replica has never swept.
func (m *memoryStore) getAttention() (*FleetSnapshot, *AttentionReport) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fleet, m.attention
}

// hashArgs flattens a field map into HSET arguments after the key.
func hashArgs(key string, fields map[string]interface{}) leasefence.Cmd {
	cmd := make(leasefence.Cmd, 0, 2+2*len(fields))
	cmd = append(cmd, "HSET", key)
	for f, v := range fields {
		cmd = append(cmd, f, v)
	}
	return cmd
}

// writeCache publishes a sweep's results to the shared Redis cache so every
// replica reads the same truth (§5.13). removed lists queues that were in the
// cache index but no longer exist in asynq:queues; their cache entries are
// deleted so a deleted queue does not linger as a ghost row on standby
// replicas. Each queue hash also carries a TTL as a safety net: if the
// sweeper dies, its rows eventually expire rather than serving forever-stale
// numbers with an honest-looking key.
//
// attentionJSON is the marshaled AttentionReport published alongside the
// snapshots (one SET, same TTL) so standby replicas serve findings that are
// byte-identical to the holder's.
//
// The publish is fence-guarded (§5.13, phase 12): with token > 0 every batch
// CASes on the sweeper role's fencing token, so a superseded ex-holder can
// never overwrite a newer holder's rows; ok=false reports exactly that.
// token 0 (SweepNow off-lease — tests, operational tooling) writes unfenced,
// preserving the documented "duplicate reads + idempotent last-writer-wins
// writes" semantics of explicit off-lease sweeps.
//
// Returns the number of Redis commands issued (for SweepStats).
func writeCache(ctx context.Context, fence *leasefence.Fence, token int64, fleet *FleetSnapshot, queues map[string]*QueueSnapshot, removed []string, attentionJSON []byte, ttl time.Duration) (int, bool, error) {
	ttlMs := ttl.Milliseconds()
	cmds := make([]leasefence.Cmd, 0, 2*len(queues)+2*len(removed)+4)
	names := make([]interface{}, 0, len(queues))
	for name, snap := range queues {
		cmds = append(cmds,
			hashArgs(queueCacheKey(name), snap.toHash()),
			leasefence.Cmd{"PEXPIRE", queueCacheKey(name), ttlMs})
		names = append(names, name)
	}
	if len(names) > 0 {
		add := append(leasefence.Cmd{"SADD", queueIndexKey}, names...)
		cmds = append(cmds, add)
	}
	for _, name := range removed {
		cmds = append(cmds,
			leasefence.Cmd{"SREM", queueIndexKey, name},
			leasefence.Cmd{"DEL", queueCacheKey(name)})
	}
	cmds = append(cmds,
		hashArgs(fleetCacheKey, fleet.toHash()),
		leasefence.Cmd{"PEXPIRE", fleetCacheKey, ttlMs})
	if len(attentionJSON) > 0 {
		cmds = append(cmds, leasefence.Cmd{"SET", attentionCacheKey, string(attentionJSON), "PX", ttlMs})
	}
	ok, err := fence.Exec(ctx, token, cmds)
	return len(cmds), ok, err
}

// readCache loads the fleet snapshot and all queue snapshots from the shared
// Redis cache. This is the read path for replicas that do not hold the
// sweeper lease. Returns ErrNotReady when no sweep has ever published (or the
// cache expired because the sweeper has been dead longer than the TTL).
func readCache(ctx context.Context, rc redis.UniversalClient, batchSize int) (*FleetSnapshot, []*QueueSnapshot, error) {
	fleetHash, err := rc.HGetAll(ctx, fleetCacheKey).Result()
	if err != nil {
		return nil, nil, err
	}
	if len(fleetHash) == 0 {
		return nil, nil, ErrNotReady
	}
	names, err := rc.SMembers(ctx, queueIndexKey).Result()
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(names)

	queues := make([]*QueueSnapshot, 0, len(names))
	for start := 0; start < len(names); start += batchSize {
		end := start + batchSize
		if end > len(names) {
			end = len(names)
		}
		batch := names[start:end]
		pipe := rc.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(batch))
		for i, name := range batch {
			cmds[i] = pipe.HGetAll(ctx, queueCacheKey(name))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, nil, err
		}
		for _, cmd := range cmds {
			h, err := cmd.Result()
			if err != nil || len(h) == 0 {
				// A hash can expire while its name is still in the index
				// (sweeper died mid-decay). Skip it: better a missing row
				// than a fabricated one.
				continue
			}
			queues = append(queues, queueSnapshotFromHash(h))
		}
	}
	return fleetSnapshotFromHash(fleetHash), queues, nil
}

// readAttentionCache loads the shared attention report — the read path for
// replicas that do not hold the sweeper lease. ErrNotReady when no sweep has
// ever published one (or it expired with a dead sweeper).
func readAttentionCache(ctx context.Context, rc redis.UniversalClient) (*AttentionReport, error) {
	data, err := rc.Get(ctx, attentionCacheKey).Bytes()
	if err == redis.Nil {
		return nil, ErrNotReady
	}
	if err != nil {
		return nil, err
	}
	var rep AttentionReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("decoding cached attention report: %w", err)
	}
	if rep.Findings == nil {
		rep.Findings = make([]AttentionFinding, 0)
	}
	return &rep, nil
}
