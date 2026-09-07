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
//	asynqmon:lock:<role>:fence  STRING  fencing token "<seconds>:<sequence>"
//
// The lock key format is IDENTICAL to what stats/errsig/hygiene used before
// phase 12, so a rolling deploy mixing old and new replicas still contends on
// the same lease — no behavior change to lease semantics. The fence key is
// additive.
//
// # Token format
//
// The fence key holds "<seconds>:<sequence>". <seconds> is the Redis server
// clock (TIME) at the claim, never lower than the previous token's seconds.
// <sequence> is a counter that the acquire script increments on every claim.
// A bare integer in the fence key (the pre-#52 format) reads as a sequence
// with seconds 0, so a rolling deploy keeps the claim order. The Go side
// carries the token as one int64: seconds*1e9 + sequence (see ParseToken and
// FormatToken). A later claim always has a larger int64 than an earlier
// claim.
//
// The seconds prefix closes the counter-reset hole. After a Redis data loss
// (a Basic-tier restart, a failover that drops the last writes) a plain
// counter restarts at 1, and a paused ex-holder that held token 1 passes the
// CAS. With the prefix, the token minted after the loss carries a newer
// clock second, so the ex-holder's token no longer matches.
//
// The fence key has NO TTL, on purpose. A TTL would make the key expire while
// a live holder still presents a token minted from it; every fenced write of
// that holder would then fail. The key is one small string per role, so the
// cost of keeping it is negligible. The rollback guidance lists it as an
// owned key to delete by hand.
//
// # Fenced writes
//
// Exec runs a batch of write commands inside a Lua script that first checks
// two conditions: the caller's token equals the current fence, AND the lock
// key still holds the instance ID that minted the token. The first check
// rejects a holder that a newer claimant superseded. The second check rejects
// a holder whose lease expired and that nobody has replaced yet (the token is
// still current, but the possession is gone). A write therefore requires
// current possession, not only the last-minted token. Batches are chunked;
// each chunk re-checks both conditions, so a fence loss aborts all remaining
// chunks.
//
// Wide commands (SADD, SREM, HSET, HDEL, DEL, UNLINK, RPUSH, LPUSH, ZADD,
// ZREM) are split into several commands of at most maxMembers members each
// before the script runs. Redis Lua 5.1 `unpack` fails with "too many
// results to unpack" at about 8000 arguments; the split keeps every
// redis.call far below that limit.
//
// Token 0 = explicitly unfenced: the *Now operational entry points
// (Engine.SweepNow, Indexer.SweepTailNow, hygiene Run-now) run without a
// lease by design ("running it off-lease only costs duplicate reads and
// idempotent merges") and pass token 0, which executes a plain pipeline.
// The fence protects the leased loops, not deliberate operator actions.
//
// # Redis Cluster is unsupported for the fenced writers
//
// Exec declares only the fence key and the lock key as KEYS, then runs
// redis.call on arbitrary keys (asynqmon:cache:q:*, asynqmon:series:*,
// asynqmon:sched*, asynqmon:idx:err*, asynqmon:hygiene:*). Those keys hash
// to different slots. Redis Cluster rejects a script that touches a key
// outside its declared slots with "ERR Script attempted to access a non
// local key in a cluster node" or with "CROSSSLOT". The lock key and the
// fence key of one role also hash to different slots (no hash tag). So
// every fenced write path — the stats sweeper, the error-signature indexer
// and the hygiene scheduler — fails on a cluster. The unfenced token-0
// pipeline has the same problem for its multi-key pipelines. Run these
// writers only against a standalone or Sentinel Redis. The cmd-level gate
// disables the write features when --redis-cluster-nodes is set.
package leasefence

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// execChunk bounds how many commands one fenced Lua invocation applies. Each
// chunk is atomic and fence-checked; between chunks a superseded token stops
// the remainder.
const execChunk = 100

// maxMembers bounds the member count of one wide command inside a batch
// (#52.2). 1000 members give at most 2002 Lua arguments for HSET, far below
// the unpack limit of about 8000.
const maxMembers = 1000

// tokenSeqRange is the int64 token encoding base: token = seconds *
// tokenSeqRange + sequence. The sequence wraps only after 1e9 claims per
// role; at one claim per lease TTL that takes centuries.
const tokenSeqRange = 1_000_000_000

// LockKey returns the lease key for a role ("asynqmon:lock:<role>").
func LockKey(role string) string { return "asynqmon:lock:" + role }

// FenceKey returns the fencing-token key for a role.
func FenceKey(role string) string { return LockKey(role) + ":fence" }

// Cmd is one Redis write command: name followed by arguments, keys inline
// (e.g. Cmd{"HSET", key, "field", "value"}).
type Cmd []interface{}

// Fence guards one singleton role.
type Fence struct {
	rc   redis.UniversalClient
	role string

	// holder is the instance ID of the most recent successful Acquire on
	// this Fence. Exec presents it so a write requires current possession.
	mu     sync.Mutex
	holder string
}

// New creates a Fence for a role. Cheap; it carries only the holder ID of
// the last Acquire.
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

// ParseToken decodes a fence value into the int64 token. It accepts the
// "<seconds>:<sequence>" format and the legacy bare-integer format (seconds
// 0). It returns an error for any other input.
func ParseToken(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("leasefence: empty token")
	}
	secStr, seqStr, ok := strings.Cut(s, ":")
	if !ok {
		seq, err := strconv.ParseInt(s, 10, 64)
		if err != nil || seq < 0 {
			return 0, fmt.Errorf("leasefence: bad token %q", s)
		}
		return seq, nil
	}
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil || sec < 0 {
		return 0, fmt.Errorf("leasefence: bad token %q", s)
	}
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq < 0 || seq >= tokenSeqRange {
		return 0, fmt.Errorf("leasefence: bad token %q", s)
	}
	return sec*tokenSeqRange + seq, nil
}

// FormatToken encodes an int64 token as the "<seconds>:<sequence>" fence
// value that the scripts compare against.
func FormatToken(token int64) string {
	return strconv.FormatInt(token/tokenSeqRange, 10) + ":" + strconv.FormatInt(token%tokenSeqRange, 10)
}

// TokenTime returns the Redis clock second at which a token was minted.
func TokenTime(token int64) time.Time { return time.Unix(token/tokenSeqRange, 0) }

// TokenSeq returns the claim sequence number of a token.
func TokenSeq(token int64) int64 { return token % tokenSeqRange }

// acquireScript takes the lease and mints the next fencing token atomically
// (the jobs/store.go claimScript pattern: minting inside the same script
// closes the SET-then-INCR race, so token order always equals claim order and
// a stale claimer always holds a lower token). The token is
// "<seconds>:<sequence>": seconds from the Redis clock, never below the
// previous token's seconds; sequence = previous sequence + 1. A legacy bare
// integer counts as the previous sequence. Returns the token string, or 0
// when the lease is held by someone else.
var acquireScript = redis.NewScript(`
if not redis.call("SET", KEYS[1], ARGV[1], "NX", "PX", ARGV[2]) then
	return 0
end
local prevSec, prevSeq = 0, 0
local cur = redis.call("GET", KEYS[2])
if cur then
	local s, q = string.match(cur, "^(%d+):(%d+)$")
	if s then
		prevSec, prevSeq = tonumber(s), tonumber(q)
	else
		prevSeq = tonumber(cur) or 0
	end
end
local sec = tonumber(redis.call("TIME")[1])
if sec < prevSec then
	sec = prevSec
end
local tok = sec .. ":" .. (prevSeq + 1)
redis.call("SET", KEYS[2], tok)
return tok`)

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
// still the role's current fence AND the caller still holds the lease, then
// apply every encoded command. KEYS[1] = fence key; KEYS[2] = lock key.
// ARGV[1] = token; ARGV[2] = holder instance ID; ARGV[3] = command count;
// then per command: argc followed by argc strings (command name first).
var execScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return 0
end
if redis.call("GET", KEYS[2]) ~= ARGV[2] then
	return 0
end
local i = 4
local n = tonumber(ARGV[3])
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
// error) when another instance holds the lease. The Fence remembers
// instanceID; Exec presents it as the required lease holder.
func (f *Fence) Acquire(ctx context.Context, instanceID string, ttl time.Duration) (int64, error) {
	res, err := acquireScript.Run(ctx, f.rc,
		[]string{LockKey(f.role), FenceKey(f.role)}, instanceID, ttl.Milliseconds()).Result()
	if err != nil {
		return 0, err
	}
	switch v := res.(type) {
	case int64:
		if v == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("leasefence: unexpected acquire reply %d", v)
	case string:
		token, err := ParseToken(v)
		if err != nil {
			return 0, err
		}
		f.mu.Lock()
		f.holder = instanceID
		f.mu.Unlock()
		return token, nil
	default:
		return 0, fmt.Errorf("leasefence: unexpected acquire reply %T", res)
	}
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
// fencing token AND the lock key still holds the instance that acquired it
// through this Fence. Commands are chunked (execChunk per atomic Lua call),
// each chunk re-checking the fence. Wide commands are split to maxMembers
// members each (see splitWide). Returns false when the fence moved on or
// the lease is gone — the caller must stand down immediately; earlier chunks
// of the same batch may already be applied (chunk boundaries are the same
// batch-write boundaries the jobs runner has).
//
// token 0 executes an explicitly UNFENCED plain pipeline — the documented
// escape hatch for off-lease *Now operational calls.
func (f *Fence) Exec(ctx context.Context, token int64, cmds []Cmd) (bool, error) {
	if len(cmds) == 0 {
		return true, nil
	}
	cmds = splitWide(cmds)
	if token == 0 {
		pipe := f.rc.Pipeline()
		for _, c := range cmds {
			pipe.Do(ctx, c...)
		}
		_, err := pipe.Exec(ctx)
		return true, err
	}
	f.mu.Lock()
	holder := f.holder
	f.mu.Unlock()
	for start := 0; start < len(cmds); start += execChunk {
		end := start + execChunk
		if end > len(cmds) {
			end = len(cmds)
		}
		chunk := cmds[start:end]
		// No capacity hint: chunk is execChunk-bounded, and a data-derived
		// capacity is an overflow liability for no measurable gain.
		args := make([]interface{}, 0)
		args = append(args, FormatToken(token), holder, strconv.Itoa(len(chunk)))
		for _, c := range chunk {
			args = append(args, strconv.Itoa(len(c)))
			args = append(args, c...)
		}
		n, err := execScript.Run(ctx, f.rc, []string{FenceKey(f.role), LockKey(f.role)}, args...).Int()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, nil
		}
	}
	return true, nil
}

// wideShape describes how a wide command lays out its members: fixed =
// arguments per member (1 for SADD, 2 for HSET); zaddOpts = true when option
// flags (NX XX GT LT CH INCR) may precede the score/member pairs; noKey =
// true when the command has no key argument (DEL, UNLINK: every argument is
// a member).
type wideShape struct {
	fixed    int
	zaddOpts bool
	noKey    bool
}

var wideCommands = map[string]wideShape{
	"SADD":   {fixed: 1},
	"SREM":   {fixed: 1},
	"HDEL":   {fixed: 1},
	"RPUSH":  {fixed: 1},
	"LPUSH":  {fixed: 1},
	"ZREM":   {fixed: 1},
	"HSET":   {fixed: 2},
	"ZADD":   {fixed: 2, zaddOpts: true},
	"DEL":    {fixed: 1, noKey: true},
	"UNLINK": {fixed: 1, noKey: true},
}

var zaddOptions = map[string]bool{"NX": true, "XX": true, "GT": true, "LT": true, "CH": true, "INCR": true}

// splitWide returns cmds with every wide command of more than maxMembers
// members replaced by several commands of at most maxMembers members each,
// in the original order. Commands that are not wide, or not wider than the
// limit, pass through unchanged (and the input slice is returned as is when
// nothing was split). The split preserves semantics: SADD, SREM, HSET, HDEL,
// ZADD, ZREM, DEL and UNLINK are member-wise idempotent in order, and LPUSH
// and RPUSH keep their push order across the pieces.
func splitWide(cmds []Cmd) []Cmd {
	var out []Cmd
	for i, c := range cmds {
		pieces := splitOne(c)
		if out == nil {
			if len(pieces) == 1 {
				continue
			}
			out = make([]Cmd, 0, len(cmds)+len(pieces))
			out = append(out, cmds[:i]...)
		}
		out = append(out, pieces...)
	}
	if out == nil {
		return cmds
	}
	return out
}

// splitOne splits one command when it is wide and above the member limit.
func splitOne(c Cmd) []Cmd {
	if len(c) < 2 {
		return []Cmd{c}
	}
	name, ok := c[0].(string)
	if !ok {
		return []Cmd{c}
	}
	shape, wide := wideCommands[strings.ToUpper(name)]
	if !wide {
		return []Cmd{c}
	}
	// prefix = command name (+ key) (+ ZADD options); members follow.
	prefixLen := 1
	if !shape.noKey {
		prefixLen = 2
	}
	if shape.zaddOpts {
		for prefixLen < len(c) {
			s, isStr := c[prefixLen].(string)
			if !isStr || !zaddOptions[strings.ToUpper(s)] {
				break
			}
			prefixLen++
		}
	}
	if prefixLen > len(c) {
		return []Cmd{c}
	}
	members := c[prefixLen:]
	if len(members)%shape.fixed != 0 || len(members) <= maxMembers*shape.fixed {
		return []Cmd{c}
	}
	prefix := c[:prefixLen]
	step := maxMembers * shape.fixed
	pieces := make([]Cmd, 0, (len(members)+step-1)/step)
	for start := 0; start < len(members); start += step {
		end := start + step
		if end > len(members) {
			end = len(members)
		}
		piece := make(Cmd, 0, prefixLen+(end-start))
		piece = append(piece, prefix...)
		piece = append(piece, members[start:end]...)
		pieces = append(pieces, piece)
	}
	return pieces
}

// StandDown clears an engine's holder state after a fenced write with
// `rejected` was refused — but only when `rejected` is still the token the
// engine holds. It returns true when it cleared the state. It returns false
// when the engine already holds a newer token: the engine re-acquired the
// lease after the rejected write started (its own lease expired mid-run and
// it won the next claim), so the rejection is stale and the engine keeps its
// current lease (#52.1). The compare-and-swap on the token makes the check
// atomic against a concurrent leaseTick.
func StandDown(token *int64, holding *int32, rejected int64) bool {
	if rejected == 0 {
		return false
	}
	if !atomic.CompareAndSwapInt64(token, rejected, 0) {
		return false
	}
	atomic.StoreInt32(holding, 0)
	return true
}

// HolderInfo describes a role's current lease for the Settings › Health
// readout (§3.12).
type HolderInfo struct {
	Role string
	// Holder is the lease value (instance ID); "" when no one holds it.
	Holder string
	// TTL is the lease's remaining time; 0 when unheld.
	TTL time.Duration
	// Fence is the current fencing token (int64 encoding; see ParseToken).
	Fence int64
	// FenceRaw is the fence key value as stored ("<seconds>:<sequence>").
	FenceRaw string
}

// Holder reads the role's current lease holder, TTL and fence token (3
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
		info.FenceRaw = v
		info.Fence, _ = ParseToken(v)
	}
	return info, nil
}
