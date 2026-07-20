package asynqmon

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - helpers for reading per-task timing data that asynq's Inspector API does
//     not expose
// ****************************************************************************

// asynq records when a task entered the pending state as a "pending_since"
// field (Unix nanoseconds) on the task's hash, and deletes the field when a
// worker dequeues the task. The Inspector only surfaces it in aggregate, as
// QueueInfo.Latency (the age of the oldest pending task), so we read the field
// directly to show a per-task queued duration.
//
// Both the key layout and the field name are asynq internals rather than
// public API. Every caller therefore treats a miss as "unknown" and renders a
// placeholder, so an asynq upgrade that changes the layout degrades to a blank
// column instead of a broken page.

// taskKey returns the redis key for a task's hash. The queue name is wrapped in
// a hash tag so that all keys for a queue land on the same cluster slot.
func taskKey(qname, id string) string {
	return fmt.Sprintf("asynq:{%s}:t:%s", qname, id)
}

// pendingSinceTimes looks up when each of the given tasks entered the pending
// state. IDs with no recorded value are absent from the returned map.
//
// All keys share the queue's cluster slot, so the pipeline is a single
// round-trip even against a Redis Cluster.
func pendingSinceTimes(ctx context.Context, rc redis.UniversalClient, qname string, ids []string) map[string]time.Time {
	out := make(map[string]time.Time, len(ids))
	if rc == nil || len(ids) == 0 {
		return out
	}
	pipe := rc.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGet(ctx, taskKey(qname, id), "pending_since")
	}
	// redis.Nil is returned per-command for missing fields and surfaces here as
	// the pipeline error; the per-command results are still populated, so the
	// error is inspected per command below rather than aborting.
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return out
	}
	for i, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil {
			continue
		}
		nanos, err := strconv.ParseInt(v, 10, 64)
		if err != nil || nanos <= 0 {
			continue
		}
		out[ids[i]] = time.Unix(0, nanos)
	}
	return out
}
