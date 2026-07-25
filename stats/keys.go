// Package stats implements the Fleet Console stats engine (build contract
// §5.1, phase 2 / tier 1): a leased background sweeper that reads per-queue
// counters from Redis with a bounded, pipelined command set — bypassing
// Inspector.GetQueueInfo entirely (no group iteration, no MEMORY USAGE) — and
// publishes snapshots to a shared Redis cache plus an in-process copy.
package stats

import (
	"fmt"
	"time"
)

// ****************************************************************************
// This file defines:
//   - asynq's internal Redis key formats, mirrored locally
//   - asynqmon-owned keys for the stats cache and the sweeper lease
// ****************************************************************************

// asynqmon may not import asynq's internal packages, so the key formats below
// are mirrored deliberately from asynq's internals, version-pinned to the
// asynq release this module depends on:
//
//	github.com/hibiken/asynq@v0.24.1/internal/base/base.go
//
// These are asynq internals rather than public API, and internals may change
// between asynq releases. Every reader of these keys therefore treats a miss
// (key absent, wrong type, unparsable value) as "unknown" and degrades to a
// zero/blank value instead of failing the sweep, so an asynq upgrade that
// changes the layout degrades to stale-but-labeled numbers, not a broken
// console. The stats test suite seeds Redis through the real asynq client so
// any layout drift is caught at test time, not in production.
//
// Semantics worth pinning down (verified against asynq v0.24.1 sources):
//   - pending/active are LISTs. Enqueue LPUSHes, dequeue RPOPLPUSHes, so the
//     OLDEST pending task sits at index -1 (this is also how asynq's own
//     CurrentStats computes queue latency).
//   - lease is a ZSET whose scores are lease-expiry Unix times in seconds;
//     asynq workers hold 30s leases. A member with score < now is a lease
//     that expired without the task being acked — an orphan candidate once
//     it stays expired beyond a grace period.
//   - paused is a plain STRING holding the pause time as Unix seconds
//     (SETNX by asynq, no TTL). Existence == paused; value == paused-since.
//   - processed:<date>/failed:<date> daily counters use the UTC date.
//   - the task hash's "pending_since" field is Unix nanoseconds, written on
//     enqueue and deleted on dequeue.

const (
	// allQueuesKey is the SET of all known queue names ("asynq:queues").
	allQueuesKey = "asynq:queues"
)

// queueKeyPrefix returns the prefix for all keys of the given queue. The queue
// name is wrapped in a hash tag so all of a queue's keys share one cluster slot.
func queueKeyPrefix(qname string) string {
	return fmt.Sprintf("asynq:{%s}:", qname)
}

// pendingKey returns the key of the pending LIST.
func pendingKey(qname string) string { return queueKeyPrefix(qname) + "pending" }

// activeKey returns the key of the active LIST.
func activeKey(qname string) string { return queueKeyPrefix(qname) + "active" }

// scheduledKey returns the key of the scheduled ZSET (scores = process-at Unix seconds).
func scheduledKey(qname string) string { return queueKeyPrefix(qname) + "scheduled" }

// retryKey returns the key of the retry ZSET (scores = next-retry Unix seconds).
func retryKey(qname string) string { return queueKeyPrefix(qname) + "retry" }

// archivedKey returns the key of the archived (dead-letter) ZSET.
func archivedKey(qname string) string { return queueKeyPrefix(qname) + "archived" }

// completedKey returns the key of the completed ZSET (scores = expire-at Unix seconds).
func completedKey(qname string) string { return queueKeyPrefix(qname) + "completed" }

// leaseKey returns the key of the lease ZSET (scores = lease-expiry Unix seconds).
func leaseKey(qname string) string { return queueKeyPrefix(qname) + "lease" }

// pausedKey returns the key of the paused marker STRING (value = paused-at Unix seconds).
func pausedKey(qname string) string { return queueKeyPrefix(qname) + "paused" }

// processedKey returns the key of the daily processed counter for the given day (UTC).
func processedKey(qname string, t time.Time) string {
	return fmt.Sprintf("%sprocessed:%s", queueKeyPrefix(qname), t.UTC().Format("2006-01-02"))
}

// failedKey returns the key of the daily failed counter for the given day (UTC).
func failedKey(qname string, t time.Time) string {
	return fmt.Sprintf("%sfailed:%s", queueKeyPrefix(qname), t.UTC().Format("2006-01-02"))
}

// taskKey returns the key of a task's HASH (fields include "pending_since").
func taskKey(qname, id string) string {
	return fmt.Sprintf("%st:%s", queueKeyPrefix(qname), id)
}

// allGroupsKey returns the SET of group names used in the given queue.
func allGroupsKey(qname string) string { return queueKeyPrefix(qname) + "groups" }

// ----------------------------------------------------------------------------
// asynqmon-owned keys. All new state lives under the "asynqmon:" prefix (§5)
// so it can never collide with — and is trivially distinguishable from —
// asynq's own keys. asynqmon never writes under "asynq:".
// ----------------------------------------------------------------------------

const (
	// queueCachePrefix + <qname> is the HASH holding a queue's cached snapshot.
	queueCachePrefix = "asynqmon:cache:q:"

	// fleetCacheKey is the HASH holding the fleet-aggregate snapshot.
	fleetCacheKey = "asynqmon:cache:fleet"

	// queueIndexKey is the SET of queue names present in the cache. It exists
	// so replicas without the lease can enumerate cached queues without a
	// SCAN. A per-column sorted index is deliberately NOT maintained in tier 1:
	// sorting happens in-process over the full cached set, which is fine at
	// the tier-1 ceiling of ~2k queues (§5.1 tiering table; revisit in phase 12).
	queueIndexKey = "asynqmon:cache:queues"

	// sweeperLockKey is the singleton lease guarding the stats sweeper role
	// (§5.13): SET NX PX by the winning replica, renewed at ~1/3 TTL. Loser
	// replicas stand by and serve reads from the Redis cache.
	sweeperLockKey = "asynqmon:lock:stats"
)

// queueCacheKey returns the cache HASH key for the given queue.
func queueCacheKey(qname string) string { return queueCachePrefix + qname }
