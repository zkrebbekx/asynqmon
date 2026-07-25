package hygiene

import (
	"fmt"
	"time"
)

// ****************************************************************************
// This file mirrors the asynq internal Redis key formats the bounded direct
// reads need — the same version-pinned rationale as stats/keys.go and the
// errsig package: asynqmon may not import asynq internals, so the formats are
// mirrored from github.com/hibiken/asynq@v0.24.1/internal/base/base.go.
// Readers treat a miss (absent key, wrong type) as "unknown/zero", never as a
// failure, so an asynq layout change degrades to honest gaps, not a broken
// report. The root-package integration suite seeds through the real asynq
// client, tripwiring any drift.
//
// Semantics relied on here (verified against asynq v0.24.1):
//   - archived is a ZSET scored by died-at Unix seconds.
//   - completed is a ZSET scored by expire-at Unix seconds (retention);
//     asynq's janitor deletes members whose score passed.
//   - pending/active are LISTs of task IDs (LPUSH in, RPOPLPUSH out).
//   - scheduled/retry are ZSETs of task IDs.
//   - the task HASH's "msg" field holds the protobuf-encoded TaskMessage
//     (type, payload, options) — its length is the honest proxy for a task's
//     payload weight.
//   - processed:<date>/failed:<date> daily counters use UTC dates and carry a
//     90-day TTL, so a 30-day movement window is always resolvable.
// ****************************************************************************

const allQueuesKey = "asynq:queues"

func queueKeyPrefix(qname string) string { return fmt.Sprintf("asynq:{%s}:", qname) }
func pendingKey(qname string) string     { return queueKeyPrefix(qname) + "pending" }
func activeKey(qname string) string      { return queueKeyPrefix(qname) + "active" }
func scheduledKey(qname string) string   { return queueKeyPrefix(qname) + "scheduled" }
func retryKey(qname string) string       { return queueKeyPrefix(qname) + "retry" }
func archivedKey(qname string) string    { return queueKeyPrefix(qname) + "archived" }
func completedKey(qname string) string   { return queueKeyPrefix(qname) + "completed" }
func taskKey(qname, id string) string    { return fmt.Sprintf("%st:%s", queueKeyPrefix(qname), id) }

func processedKey(qname string, t time.Time) string {
	return fmt.Sprintf("%sprocessed:%s", queueKeyPrefix(qname), t.UTC().Format("2006-01-02"))
}

func failedKey(qname string, t time.Time) string {
	return fmt.Sprintf("%sfailed:%s", queueKeyPrefix(qname), t.UTC().Format("2006-01-02"))
}
