package errsig

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - the singleton lease guarding the error-indexer role
//
// §5.13 lists the error indexer as its OWN singleton role, separate from the
// stats sweeper — so rather than hooking a callback into the stats engine's
// sweep (the scheduler.go pattern), the indexer runs its own loop under its
// own lease ("asynqmon:lock:errsig"). Rationale, documented per the build
// note: the tail feeder's 15s cadence and the sampler's 60s cadence are
// independent of the stats sweep interval (which operators tune per fleet),
// the indexer must keep running when the stats engine is disabled, and a
// slow tail catch-up (first run over a deep archive) must never delay the
// stats sweep. Same SET NX PX + compare-and-set Lua pattern as stats/lease.go;
// no fencing token — a brief two-indexer overlap after a handover only costs
// duplicate reads plus an idempotent last-writer-wins merge write.
// ****************************************************************************

const indexerLockKey = "asynqmon:lock:errsig"

var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

func tryAcquireLease(ctx context.Context, rc redis.UniversalClient, instanceID string, ttl time.Duration) (bool, error) {
	return rc.SetNX(ctx, indexerLockKey, instanceID, ttl).Result()
}

func renewLease(ctx context.Context, rc redis.UniversalClient, instanceID string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, rc, []string{indexerLockKey}, instanceID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func releaseLease(ctx context.Context, rc redis.UniversalClient, instanceID string) error {
	return releaseScript.Run(ctx, rc, []string{indexerLockKey}, instanceID).Err()
}
