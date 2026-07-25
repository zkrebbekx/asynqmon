// Package observe provides an OPT-IN asynq server middleware that records
// per-attempt run history — the two durations the asynqmon dashboard cannot
// otherwise show, because asynq itself stores neither:
//
//   - run duration of finished tasks (asynq keeps no start/end for
//     completed or archived tasks), and
//   - per-attempt history (asynq overwrites LastErr on every retry and
//     deletes pending_since at dequeue).
//
// Workers that adopt the middleware write bounded, asynqmon-owned records
// to Redis; the asynqmon dashboard surfaces them as "observed" data.
// Coverage is honest: only attempts that ran through an adopting worker are
// recorded — the dashboard labels observed data as such and never fabricates
// history for attempts that predate adoption.
//
// # Integration
//
//	rc := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	mux := asynq.NewServeMux()
//	mux.Use(observe.Middleware(rc))
//	mux.HandleFunc("email:send", handleEmailSend)
//	srv.Run(mux)
//
// The redis.UniversalClient must point at the same Redis (and DB) the asynq
// server uses, so the dashboard reading that DB finds the records.
//
// # What is recorded
//
// One JSON record per attempt: attempt number (GetRetryCount+1), start time,
// duration, outcome ("ok", "error" with the message capped at 500 bytes, or
// "panic" — recorded, then rethrown so asynq's own recovery semantics are
// untouched), worker host:pid, and the task's queue, id and type.
//
// # Storage bounds
//
// Records live in asynqmon-owned keys, sized and expiring by design:
//
//	asynqmon:obs:<queue>:<task_id>      LIST of per-attempt JSON records,
//	                                    newest first, LTRIMmed to the
//	                                    attempt cap (default 30)
//	asynqmon:obs:sum:<queue>:<task_id>  HASH summary: first_seen,
//	                                    total_attempts, last_duration_ms,
//	                                    last_outcome, total_busy_ms
//
// Both keys carry a TTL (default 7 days) refreshed on every write. Tune with
// WithAttemptCap, WithTTL and WithKeyPrefix. Note the bundled dashboard reads
// the default prefix; change it only when you also run your own reader.
//
// # Failure behavior
//
// Writes are best-effort: a recording failure (Redis down, timeout) NEVER
// fails or delays the task beyond a 2s write timeout. Dropped writes are
// counted; expose DroppedWrites via your own metrics if you want visibility.
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	// DefaultKeyPrefix is the key namespace the bundled asynqmon dashboard
	// reads. All observe keys live under it: "<prefix><queue>:<task_id>"
	// (attempts) and "<prefix>sum:<queue>:<task_id>" (summary).
	DefaultKeyPrefix = "asynqmon:obs:"

	// DefaultAttemptCap bounds the per-task attempt list.
	DefaultAttemptCap = 30

	// DefaultTTL bounds how long records outlive their last write.
	DefaultTTL = 7 * 24 * time.Hour

	// maxErrorLen caps the stored error/panic message, in bytes.
	maxErrorLen = 500

	// writeTimeout bounds the post-attempt Redis write. The task's own
	// context is NOT used: it may already be canceled or past deadline
	// (often exactly why the attempt failed), and recording must still work.
	writeTimeout = 2 * time.Second
)

// AttemptRecord is one observed handler run, as stored (JSON) in the
// attempts list. Field names are a wire contract with the asynqmon
// dashboard's /observed endpoint.
type AttemptRecord struct {
	N          int    `json:"n"`     // attempt number: GetRetryCount+1
	Start      string `json:"start"` // RFC3339Nano, UTC
	DurationMs int64  `json:"duration_ms"`
	Outcome    string `json:"outcome"` // "ok" | "error" | "panic"
	Error      string `json:"error,omitempty"`
	Worker     string `json:"worker"` // host:pid
	Queue      string `json:"queue"`
	TaskID     string `json:"task_id"`
	Type       string `json:"type"`
}

// AttemptsKey returns the Redis LIST key holding a task's attempt records.
func AttemptsKey(prefix, queue, taskID string) string {
	return prefix + queue + ":" + taskID
}

// SummaryKey returns the Redis HASH key holding a task's run summary.
func SummaryKey(prefix, queue, taskID string) string {
	return prefix + "sum:" + queue + ":" + taskID
}

type config struct {
	ttl        time.Duration
	attemptCap int
	keyPrefix  string
}

// Option customizes the middleware's storage bounds.
type Option func(*config)

// WithTTL sets how long a task's records outlive their last write
// (default 7 days). Non-positive values are ignored.
func WithTTL(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithAttemptCap bounds the per-task attempt list (default 30). The summary's
// total_attempts still counts every observed attempt. Non-positive values are
// ignored.
func WithAttemptCap(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.attemptCap = n
		}
	}
}

// WithKeyPrefix changes the key namespace (default "asynqmon:obs:"). The
// bundled dashboard reads the default prefix — override only when running
// your own reader against the custom namespace.
func WithKeyPrefix(p string) Option {
	return func(c *config) {
		if p != "" {
			c.keyPrefix = p
		}
	}
}

// droppedWrites counts attempt records that could not be written (marshal or
// Redis failure). Recording is best-effort by contract, so failures are
// counted, never surfaced to the task.
var droppedWrites uint64

// DroppedWrites reports how many attempt records this process failed to
// write. Export it through your own metrics for visibility into recording
// health.
func DroppedWrites() uint64 { return atomic.LoadUint64(&droppedWrites) }

// Middleware returns an asynq middleware that records one AttemptRecord per
// handler run, usable via (*asynq.ServeMux).Use. See the package
// documentation for what is recorded and the storage bounds.
//
// A panic in the wrapped handler is recorded (outcome "panic"), then
// rethrown, so asynq's own panic recovery — the attempt counts as a failure —
// is preserved. When the context carries no asynq task metadata (i.e. the
// handler is not running inside an asynq server), the middleware records
// nothing and just delegates.
func Middleware(rc redis.UniversalClient, opts ...Option) func(asynq.Handler) asynq.Handler {
	cfg := config{ttl: DefaultTTL, attemptCap: DefaultAttemptCap, keyPrefix: DefaultKeyPrefix}
	for _, opt := range opts {
		opt(&cfg)
	}
	worker := workerID()

	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			taskID, okID := asynq.GetTaskID(ctx)
			queue, okQ := asynq.GetQueueName(ctx)
			if !okID || !okQ {
				// Not inside an asynq server — nothing addressable to record.
				return next.ProcessTask(ctx, t)
			}
			rec := AttemptRecord{
				N:      1,
				Worker: worker,
				Queue:  queue,
				TaskID: taskID,
				Type:   t.Type(),
			}
			if n, ok := asynq.GetRetryCount(ctx); ok {
				rec.N = n + 1
			}

			start := time.Now()
			rec.Start = start.UTC().Format(time.RFC3339Nano)

			panicking := true
			defer func() {
				if !panicking {
					return
				}
				// Record, then rethrow: the recover here exists only to
				// observe the value; asynq's processor still sees the panic
				// and fails the attempt exactly as without this middleware.
				p := recover()
				rec.DurationMs = time.Since(start).Milliseconds()
				rec.Outcome = "panic"
				rec.Error = truncateErr(fmt.Sprintf("%v", p))
				record(rc, cfg, rec)
				panic(p)
			}()
			err := next.ProcessTask(ctx, t)
			panicking = false

			rec.DurationMs = time.Since(start).Milliseconds()
			if err != nil {
				rec.Outcome = "error"
				rec.Error = truncateErr(err.Error())
			} else {
				rec.Outcome = "ok"
			}
			record(rc, cfg, rec)
			return err
		})
	}
}

// record writes one attempt + summary update, best-effort. It runs on its own
// short-timeout background context — never the task's, which may already be
// canceled — and NEVER returns an error to the caller.
func record(rc redis.UniversalClient, cfg config, rec AttemptRecord) {
	data, err := json.Marshal(rec)
	if err != nil {
		atomic.AddUint64(&droppedWrites, 1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	ak := AttemptsKey(cfg.keyPrefix, rec.Queue, rec.TaskID)
	sk := SummaryKey(cfg.keyPrefix, rec.Queue, rec.TaskID)
	pipe := rc.Pipeline()
	pipe.LPush(ctx, ak, data)
	pipe.LTrim(ctx, ak, 0, int64(cfg.attemptCap-1))
	pipe.Expire(ctx, ak, cfg.ttl)
	pipe.HSetNX(ctx, sk, "first_seen", rec.Start)
	pipe.HIncrBy(ctx, sk, "total_attempts", 1)
	pipe.HIncrBy(ctx, sk, "total_busy_ms", rec.DurationMs)
	pipe.HSet(ctx, sk, "last_duration_ms", rec.DurationMs, "last_outcome", rec.Outcome)
	pipe.Expire(ctx, sk, cfg.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		atomic.AddUint64(&droppedWrites, 1)
	}
}

// truncateErr caps a message at maxErrorLen bytes without splitting a UTF-8
// rune at the cut.
func truncateErr(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	s = s[:maxErrorLen]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}
