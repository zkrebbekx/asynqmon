// Package leasefence is the shared singleton-role lease + fencing-token
// helper (build contract §5.13, phase 12). It extracts the jobs/store.go
// claim pattern — a SET NX PX lease whose acquisition Lua atomically mints a
// monotonically increasing fencing token, and write paths that CAS on that
// token — so the stats sweeper, error-signature indexer and hygiene scheduler
// share one hardened implementation instead of three lease copies. The
// bulk-job runner keeps its own per-job claim implementation (its fence lives
// inside each job hash and its guarded write is job-shaped); this package
// covers the process-singleton roles.
//
// Keys (per role):
//
//	asynqmon:lock:<role>        STRING  lease (value = holder instance ID)
//	asynqmon:lock:<role>:fence  STRING  fencing counter (INCR on every claim)
//
// The lock key format is IDENTICAL to what stats/errsig/hygiene used before
// phase 12, so a rolling deploy mixing old and new replicas still contends on
// the same lease — no behavior change to lease semantics. The fence key is
// additive.
//
// Fenced writes: Exec runs a batch of write commands inside a Lua script that
// first compares the caller's token against the current fence — a stale
// holder (paused, partitioned, superseded after lease expiry) can never write
// after a newer claimant minted a higher token. Batches are chunked; each
// chunk re-checks the fence (the jobs "every batch write is preceded by a
// token check" discipline), so a fence loss aborts all remaining chunks.
//
// Token 0 = explicitly unfenced: the *Now operational entry points
// (Engine.SweepNow, Indexer.SweepTailNow, hygiene Run-now) run without a
// lease by design ("running it off-lease only costs duplicate reads and
// idempotent merges") and pass token 0, which executes a plain pipeline.
// The fence protects the leased loops, not deliberate operator actions.
//
// Cluster note: like every existing asynqmon:* write path, fenced batches
// touch multiple non-hash-tagged keys and therefore assume a non-cluster
// Redis (standalone/sentinel) — the same constraint the cache pipelines
// already carry.
package leasefence

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// execChunk bounds how many commands one fenced Lua invocation applies. Each
// chunk is atomic and fence-checked; between chunks a superseded token stops
// the remainder.
const execChunk = 100

// LockKey returns the lease key for a role ("asynqmon:lock:<role>").
func LockKey(role string) string { return "asynqmon:lock:" + role }

// FenceKey returns the fencing-counter key for a role.
func FenceKey(role string) string { return LockKey(role) + ":fence" }

// Cmd is one Redis write command: name followed by arguments, keys inline
// (e.g. Cmd{"HSET", key, "field", "value"}).
type Cmd []interface{}

// Fence guards one singleton role.
type Fence struct {
	rc   redis.UniversalClient
	role string
}

// New creates a Fence for a role. Stateless and cheap.
func New(rc redis.UniversalClient, role string) *Fence {
	if rc == nil {
		panic("leasefence.New: redis client is required")
	}
	if role == "" {
		panic("leasefence.New: role is required")
	}
	return &Fence{rc: rc, role: role}
}

// Role returns the role this fence guards.
func (f *Fence) Role() string { return f.role }

// acquireScript takes the lease and mints the next fencing token atomically
// (the jobs/store.go claimScript pattern: minting inside the same script
// closes the SET-then-INCR race, so token order always equals claim order and
// a stale claimer always holds a lower token). Returns the token, or 0 when
// the lease is held by someone else.
var acquireScript = redis.NewScript(`
if redis.call("SET", KEYS[1], ARGV[1], "NX", "PX", ARGV[2]) then
	return redis.call("INCR", KEYS[2])
end
return 0`)

// renewScript extends the lease only while we still hold it.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// releaseScript drops the lease only if we hold it, so a stopping replica
// hands over immediately instead of making the standby wait out the TTL.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// execScript is the generic fenced batch: abort unless the caller's token is
// still the role's current fence, then apply every encoded command. ARGV[1] =
// token; ARGV[2] = command count; then per command: argc followed by argc
// strings (command name first).
var execScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return 0
end
local i = 3
local n = tonumber(ARGV[2])
for c = 1, n do
	local argc = tonumber(ARGV[i]); i = i + 1
	local cmd = {}
	for a = 1, argc do
		cmd[a] = ARGV[i]; i = i + 1
	end
	redis.call(unpack(cmd))
end
return 1`)

// Acquire attempts to take the role's lease. On success it returns the
// freshly-minted fencing token (> 0) that fenced writes must present; 0 (nil
// error) when another instance holds the lease.
func (f *Fence) Acquire(ctx context.Context, instanceID string, ttl time.Duration) (int64, error) {
	return acquireScript.Run(ctx, f.rc,
		[]string{LockKey(f.role), FenceKey(f.role)}, instanceID, ttl.Milliseconds()).Int64()
}

// Renew extends the lease; false (nil error) means the lease was lost.
func (f *Fence) Renew(ctx context.Context, instanceID string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, f.rc, []string{LockKey(f.role)}, instanceID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Release gives up the lease if this instance holds it. Best-effort; the TTL
// is the backstop.
func (f *Fence) Release(ctx context.Context, instanceID string) error {
	return releaseScript.Run(ctx, f.rc, []string{LockKey(f.role)}, instanceID).Err()
}

// Exec applies cmds if (and only if) token is still the role's current
// fencing token. Commands are chunked (execChunk per atomic Lua call), each
// chunk re-checking the fence. Returns false when the fence moved on — the
// caller must stand down immediately; earlier chunks of the same batch may
// already be applied (chunk boundaries are the same batch-write boundaries
// the jobs runner has).
//
// token 0 executes an explicitly UNFENCED plain pipeline — the documented
// escape hatch for off-lease *Now operational calls.
func (f *Fence) Exec(ctx context.Context, token int64, cmds []Cmd) (bool, error) {
	if len(cmds) == 0 {
		return true, nil
	}
	if token == 0 {
		pipe := f.rc.Pipeline()
		for _, c := range cmds {
			pipe.Do(ctx, c...)
		}
		_, err := pipe.Exec(ctx)
		return true, err
	}
	for start := 0; start < len(cmds); start += execChunk {
		end := start + execChunk
		if end > len(cmds) {
			end = len(cmds)
		}
		chunk := cmds[start:end]
		// No capacity hint: chunk is execChunk-bounded, and a data-derived
		// capacity is an overflow liability for no measurable gain.
		args := make([]interface{}, 0)
		args = append(args, strconv.FormatInt(token, 10), strconv.Itoa(len(chunk)))
		for _, c := range chunk {
			args = append(args, strconv.Itoa(len(c)))
			args = append(args, c...)
		}
		n, err := execScript.Run(ctx, f.rc, []string{FenceKey(f.role)}, args...).Int()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, nil
		}
	}
	return true, nil
}

// HolderInfo describes a role's current lease for the Settings › Health
// readout (§3.12).
type HolderInfo struct {
	Role string
	// Holder is the lease value (instance ID); "" when no one holds it.
	Holder string
	// TTL is the lease's remaining time; 0 when unheld.
	TTL time.Duration
	// Fence is the current fencing token (how many claims ever happened).
	Fence int64
}

// Holder reads the role's current lease holder, TTL and fence counter (3
// pipelined commands; read-only, any replica).
func (f *Fence) Holder(ctx context.Context) (HolderInfo, error) {
	pipe := f.rc.Pipeline()
	getCmd := pipe.Get(ctx, LockKey(f.role))
	ttlCmd := pipe.PTTL(ctx, LockKey(f.role))
	fenceCmd := pipe.Get(ctx, FenceKey(f.role))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return HolderInfo{Role: f.role}, err
	}
	info := HolderInfo{Role: f.role}
	if v, err := getCmd.Result(); err == nil {
		info.Holder = v
	}
	if d, err := ttlCmd.Result(); err == nil && d > 0 {
		info.TTL = d
	}
	if v, err := fenceCmd.Result(); err == nil {
		info.Fence, _ = strconv.ParseInt(v, 10, 64)
	}
	return info, nil
}
