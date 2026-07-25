package stats

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - the in-process snapshot store (zero-latency reads on the lease holder)
//   - reading and writing the shared Redis cache (reads for standby replicas)
// ****************************************************************************

// memoryStore holds the latest sweep results in process. Snapshots are
// treated as immutable once published: the sweeper always builds fresh
// structs and replaces the whole map, so readers may hold returned pointers
// across the RWMutex boundary without copies.
type memoryStore struct {
	mu        sync.RWMutex
	fleet     *FleetSnapshot
	queues    map[string]*QueueSnapshot
	lastSweep SweepStats
}

// SweepStats records the measured cost of the most recent sweep — the input
// for the phase-12 command-budget governor and the Settings › Health readout.
type SweepStats struct {
	At        time.Time
	Queues    int
	ReadCmds  int // Redis commands issued to read asynq state
	WriteCmds int // Redis commands issued to write the asynqmon cache
	Duration  time.Duration
}

func newMemoryStore() *memoryStore {
	return &memoryStore{}
}

func (m *memoryStore) replace(fleet *FleetSnapshot, queues map[string]*QueueSnapshot, stats SweepStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fleet = fleet
	m.queues = queues
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

// writeCache publishes a sweep's results to the shared Redis cache so every
// replica reads the same truth (§5.13). removed lists queues that were in the
// cache index but no longer exist in asynq:queues; their cache entries are
// deleted so a deleted queue does not linger as a ghost row on standby
// replicas. Each queue hash also carries a TTL as a safety net: if the
// sweeper dies, its rows eventually expire rather than serving forever-stale
// numbers with an honest-looking key.
//
// Returns the number of Redis commands issued (for SweepStats).
func writeCache(ctx context.Context, rc redis.UniversalClient, fleet *FleetSnapshot, queues map[string]*QueueSnapshot, removed []string, ttl time.Duration) (int, error) {
	pipe := rc.Pipeline()
	cmds := 0
	names := make([]interface{}, 0, len(queues))
	for name, snap := range queues {
		pipe.HSet(ctx, queueCacheKey(name), snap.toHash())
		pipe.PExpire(ctx, queueCacheKey(name), ttl)
		names = append(names, name)
		cmds += 2
	}
	if len(names) > 0 {
		pipe.SAdd(ctx, queueIndexKey, names...)
		cmds++
	}
	for _, name := range removed {
		pipe.SRem(ctx, queueIndexKey, name)
		pipe.Del(ctx, queueCacheKey(name))
		cmds += 2
	}
	pipe.HSet(ctx, fleetCacheKey, fleet.toHash())
	pipe.PExpire(ctx, fleetCacheKey, ttl)
	cmds += 2
	_, err := pipe.Exec(ctx)
	return cmds, err
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
