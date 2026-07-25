package hygiene

import (
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/internal/leasefence"
)

// ****************************************************************************
// The singleton lease guarding the hygiene scheduler role (§5.13), delegated
// to the shared internal/leasefence helper since phase 12 — same lock key as
// before ("asynqmon:lock:hygiene"), plus the additive fencing token: the
// SCHEDULED report persists CAS on the token, so a paused/partitioned
// ex-scheduler can never overwrite a newer holder's report with a staler one.
// Run-now stays unfenced (token 0): any replica may generate on demand by
// design, and its write is an idempotent last-writer-wins report the operator
// explicitly asked for.
// ****************************************************************************

// schedulerRole names the role; the lock key derives from it
// ("asynqmon:lock:hygiene") so it can never drift from what leasefence
// acquires.
const schedulerRole = "hygiene"

var lockKey = leasefence.LockKey(schedulerRole)

// newSchedulerFence builds the role fence for the hygiene scheduler.
func newSchedulerFence(rc redis.UniversalClient) *leasefence.Fence {
	return leasefence.New(rc, schedulerRole)
}
