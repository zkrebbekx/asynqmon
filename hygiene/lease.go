package hygiene

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// The singleton lease guarding the hygiene scheduler role (§5.13) — the same
// SET NX PX + compare-and-set renew/release pattern as the stats sweeper
// lease (stats/lease.go), on asynqmon:lock:hygiene. No fencing token: a brief
// two-scheduler overlap after a handover only risks a duplicate report
// generation, and report writes are idempotent last-writer-wins.
// ****************************************************************************

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
	return rc.SetNX(ctx, lockKey, instanceID, ttl).Result()
}

func renewLease(ctx context.Context, rc redis.UniversalClient, instanceID string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, rc, []string{lockKey}, instanceID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func releaseLease(ctx context.Context, rc redis.UniversalClient, instanceID string) error {
	return releaseScript.Run(ctx, rc, []string{lockKey}, instanceID).Err()
}
