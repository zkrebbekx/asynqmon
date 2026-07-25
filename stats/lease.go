package stats

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - the singleton lease guarding the sweeper role (§5.13)
// ****************************************************************************

// The sweeper must be a singleton across replicas — not for correctness of
// the data (snapshot writes are idempotent, last-writer-wins), but so N
// replicas do not multiply the sweep's Redis command cost by N. The lease is
// the standard SET NX PX pattern: the value is this instance's ID, renewal
// and release are compare-and-set Lua scripts so a replica can never renew or
// delete a lease another replica now holds (the classic expired-then-stolen
// race). No fencing token here: unlike the bulk-job runner, a brief
// two-sweeper overlap after a lease handover only costs duplicate reads.

// renewScript extends the lease only if we still hold it. Returns 1 on
// success, 0 if the lease vanished or belongs to someone else.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// releaseScript deletes the lease only if we hold it, so a stopping replica
// hands over immediately instead of making the standby wait out the TTL.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// tryAcquireLease attempts to take the sweeper lease. Returns true if this
// instance now holds it.
func tryAcquireLease(ctx context.Context, rc redis.UniversalClient, instanceID string, ttl time.Duration) (bool, error) {
	return rc.SetNX(ctx, sweeperLockKey, instanceID, ttl).Result()
}

// renewLease extends the lease if this instance still holds it. Returns false
// (with nil error) when the lease was lost.
func renewLease(ctx context.Context, rc redis.UniversalClient, instanceID string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, rc, []string{sweeperLockKey}, instanceID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// releaseLease gives up the lease if this instance holds it. Best-effort:
// callers ignore the error (the TTL is the backstop).
func releaseLease(ctx context.Context, rc redis.UniversalClient, instanceID string) error {
	return releaseScript.Run(ctx, rc, []string{sweeperLockKey}, instanceID).Err()
}
