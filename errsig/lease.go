package errsig

import (
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/internal/leasefence"
)

// ****************************************************************************
// This file defines:
//   - the singleton lease guarding the error-indexer role, delegated to the
//     shared internal/leasefence helper since phase 12
//
// §5.13 lists the error indexer as its OWN singleton role, separate from the
// stats sweeper — so rather than hooking a callback into the stats engine's
// sweep (the scheduler.go pattern), the indexer runs its own loop under its
// own lease ("asynqmon:lock:errsig", key unchanged across the phase-12
// migration). Rationale, documented per the build note: the tail feeder's 15s
// cadence and the sampler's 60s cadence are independent of the stats sweep
// interval (which operators tune per fleet), the indexer must keep running
// when the stats engine is disabled, and a slow tail catch-up (first run over
// a deep archive) must never delay the stats sweep.
//
// Phase 12 adds the jobs-style fencing token (§5.13): acquisition mints a
// token and both feeders' index writes CAS on it, so a paused or partitioned
// ex-holder can never merge stale accumulations over a newer holder's index.
// The *Now methods stay runnable off-lease (token 0 → explicitly unfenced),
// preserving the documented tests/tooling semantics.
// ****************************************************************************

// indexerRole names the role; the lock key derives from it
// ("asynqmon:lock:errsig") so it can never drift from what leasefence
// acquires.
const indexerRole = "errsig"

var indexerLockKey = leasefence.LockKey(indexerRole)

// newIndexerFence builds the role fence for the indexer.
func newIndexerFence(rc redis.UniversalClient) *leasefence.Fence {
	return leasefence.New(rc, indexerRole)
}
