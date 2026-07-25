package asynqmon

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/observe"
)

// ****************************************************************************
// This file defines the observed-run-history endpoint:
//
//   GET /api/queues/{qname}/tasks/{task_id}/observed
//
// It surfaces data written by the OPT-IN worker-side middleware
// github.com/hibiken/asynqmon/observe — the run durations and per-attempt
// history asynq itself does not store (LastErr is overwritten per retry,
// pending_since is deleted at dequeue, completed tasks carry no start/end).
//
// Honesty contract: records exist only for attempts that ran through an
// adopting worker. Absence is expected, not an error — the endpoint answers
// a clean 404 {"present": false} — and the frontend labels present data as
// "recorded by observe middleware". Reads only asynqmon-owned keys under the
// observe package's DEFAULT prefix; read-only-mode safe (GET).
//
// The JSON field names are a FROZEN frontend contract (ui/src/api.ts
// ObservedTaskResponse).
// ****************************************************************************

type observedAttemptJSON struct {
	N          int    `json:"n"`
	Start      string `json:"start"`
	DurationMs int64  `json:"duration_ms"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
	Worker     string `json:"worker"`
}

type observedSummaryJSON struct {
	FirstSeen      string `json:"first_seen,omitempty"`
	TotalAttempts  int64  `json:"total_attempts"`
	LastDurationMs int64  `json:"last_duration_ms"`
	LastOutcome    string `json:"last_outcome,omitempty"`
	TotalBusyMs    int64  `json:"total_busy_ms"`
}

type observedTaskResponse struct {
	Present bool `json:"present"`
	// Newest attempt first, as stored (LPUSH order). Capped at the
	// middleware's attempt cap; summary.total_attempts still counts all.
	Attempts []observedAttemptJSON `json:"attempts"`
	Summary  *observedSummaryJSON  `json:"summary"`
}

func newObservedTaskHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		qname, taskID := vars["qname"], vars["task_id"]
		ctx := r.Context()

		pipe := rc.Pipeline()
		listCmd := pipe.LRange(ctx, observe.AttemptsKey(observe.DefaultKeyPrefix, qname, taskID), 0, -1)
		sumCmd := pipe.HGetAll(ctx, observe.SummaryKey(observe.DefaultKeyPrefix, qname, taskID))
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		raw := listCmd.Val()
		sum := sumCmd.Val()

		if len(raw) == 0 && len(sum) == 0 {
			// Clean absent: the middleware is not adopted for this task's
			// worker, or the records' TTL has expired.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(observedTaskResponse{Present: false, Attempts: []observedAttemptJSON{}})
			return
		}

		resp := observedTaskResponse{Present: true, Attempts: make([]observedAttemptJSON, 0, len(raw))}
		for _, entry := range raw {
			var rec observe.AttemptRecord
			if err := json.Unmarshal([]byte(entry), &rec); err != nil {
				continue // skip malformed entries rather than failing the read
			}
			resp.Attempts = append(resp.Attempts, observedAttemptJSON{
				N:          rec.N,
				Start:      rec.Start,
				DurationMs: rec.DurationMs,
				Outcome:    rec.Outcome,
				Error:      rec.Error,
				Worker:     rec.Worker,
			})
		}
		if len(sum) > 0 {
			hint := func(field string) int64 {
				n, _ := strconv.ParseInt(sum[field], 10, 64)
				return n
			}
			resp.Summary = &observedSummaryJSON{
				FirstSeen:      sum["first_seen"],
				TotalAttempts:  hint("total_attempts"),
				LastDurationMs: hint("last_duration_ms"),
				LastOutcome:    sum["last_outcome"],
				TotalBusyMs:    hint("total_busy_ms"),
			}
		}
		writeResponseJSON(w, resp)
	}
}
