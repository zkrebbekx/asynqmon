package stats

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/internal/leasefence"
)

// ****************************************************************************
// This file defines:
//   - the singleton lease guarding the sweeper role (§5.13), delegated to the
//     shared internal/leasefence helper since phase 12
// ****************************************************************************

// The sweeper must be a singleton across replicas — not only for read cost
// (N replicas would multiply the sweep's Redis command spend by N) but, since
// phase 12, for write safety too: acquisition mints a fencing token and the
// cache publish CASes on it, so a paused or partitioned ex-holder can never
// overwrite fresher snapshots after a standby took over (the jobs/store.go
// pattern, extracted into internal/leasefence). The lock key is unchanged
// ("asynqmon:lock:stats"), so mixed-version replica sets still contend on the
// same lease.

// sweeperLockKey is the lease key ("asynqmon:lock:stats"), derived from the
// role so it can never drift from what leasefence acquires.
var sweeperLockKey = leasefence.LockKey(sweeperRole)

// newSweeperFence builds the role fence for the sweeper.
func newSweeperFence(rc redis.UniversalClient) *leasefence.Fence {
	return leasefence.New(rc, sweeperRole)
}

// tryAcquireLease attempts to take the sweeper lease. Returns the minted
// fencing token (> 0) when this instance now holds it, 0 otherwise.
func tryAcquireLease(ctx context.Context, f *leasefence.Fence, instanceID string, ttl time.Duration) (int64, error) {
	return f.Acquire(ctx, instanceID, ttl)
}

// renewLease extends the lease if this instance still holds it. Returns false
// (with nil error) when the lease was lost.
func renewLease(ctx context.Context, f *leasefence.Fence, instanceID string, ttl time.Duration) (bool, error) {
	return f.Renew(ctx, instanceID, ttl)
}

// releaseLease gives up the lease if this instance holds it. Best-effort:
// callers ignore the error (the TTL is the backstop).
func releaseLease(ctx context.Context, f *leasefence.Fence, instanceID string) error {
	return f.Release(ctx, instanceID)
}
