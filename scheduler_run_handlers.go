package asynqmon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/jobs"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// "Run scheduler entry now" (upstream hibiken/asynqmon#337):
//
//   POST /api/schedulers/{stable_key}/run
//
// Upstream cannot offer this — asynq's Inspector is read-only over scheduler
// entries. This fork has a flag-gated enqueue client (§5.10), so running an
// entry immediately is just: resolve the stable key against live entries ∪
// persisted snapshots (§5.12 — a GONE entry is precisely the one you want to
// fire by hand), reconstruct the task (type + raw payload) and the entry's
// enqueue options, and enqueue a FRESH task through the shared client. It is
// not a scheduler tick: the entry's own history/next-run is untouched, and
// schedule-shaped options (ProcessAt/ProcessIn) are deliberately skipped.
//
// Options are reconstructed from the entry's option strings (the only form a
// snapshot retains). Queue and retention ride on the snapshot's typed fields
// (parsed from the live entry at observation time, stats/scheduler.go); the
// rest — MaxRetry, Timeout, Deadline, Unique — are parsed from asynq's
// deterministic String() renderings. Anything unparsable or unsupported is
// SKIPPED, never guessed, and the response says so:
//
//   201 {"task": <taskInfo>, "applied_options": [...], "skipped_options": [...]}
//
// Gating and audit are identical to the §5.10 enqueue route: EnableEnqueue &&
// !ReadOnly (403 JSON otherwise — see the route block in handler.go),
// actor-attributed, audit verb "run_scheduler_entry" with scope = target
// queue + entry type plus the created task id.
// ****************************************************************************

// runSchedulerAuditVerb is the audit verb for #337 entries.
const runSchedulerAuditVerb = "run_scheduler_entry"

// maxRunSchedulerBodyBytes bounds the (optional) request body: it carries at
// most a free-text audit reason.
const maxRunSchedulerBodyBytes = 64 * 1024

// runSchedulerEntryRequest is the optional JSON body of POST
// /api/schedulers/{stable_key}/run. An empty body is fine.
type runSchedulerEntryRequest struct {
	// Reason is optional free text recorded on the audit entry.
	Reason string `json:"reason"`
}

// runSchedulerEntryResponse is the wire shape of a successful run. Field
// names are a frontend contract (ui/src/api-fleet.ts RunSchedulerEntryResponse).
type runSchedulerEntryResponse struct {
	// Task is the created task in the same shape as GET
	// /api/queues/{qname}/tasks/{task_id}.
	Task *taskInfo `json:"task"`
	// AppliedOptions are the entry options honored on this enqueue, in
	// asynq's option-string rendering (Queue is always present — the
	// resolved target queue).
	AppliedOptions []string `json:"applied_options"`
	// SkippedOptions are the entry options NOT applied, each suffixed with
	// the reason ("— schedule is replaced by run-now", "— unparsable", …).
	// The task ran with queue + asynq defaults for these.
	SkippedOptions []string `json:"skipped_options"`
}

// buildRunNowOptions reconstructs the asynq options for an immediate manual
// enqueue of the entry's task (#337). Queue comes from the snapshot's typed
// field (default queue handled explicitly); the remaining options are parsed
// from the entry's option strings. Unparsable or unsupported options land in
// skipped with a reason — the enqueue proceeds with what did parse.
func buildRunNowOptions(data stats.SchedulerEntryData) (opts []asynq.Option, applied, skipped []string) {
	queue := data.Queue
	if queue == "" {
		queue = stats.DefaultTargetQueue
	}
	opts = []asynq.Option{asynq.Queue(queue)}
	applied = []string{fmt.Sprintf("Queue(%q)", queue)}
	skipped = []string{}

	skip := func(optStr, reason string) {
		skipped = append(skipped, optStr+" — "+reason)
	}

	for _, o := range data.Opts {
		name, arg, ok := splitOptString(o)
		if !ok {
			skip(o, "unparsable")
			continue
		}
		switch name {
		case "Queue":
			// Already applied from the snapshot's typed field above; the
			// string form is redundant, not skipped.
			continue
		case "Retention":
			d, err := time.ParseDuration(arg)
			if err != nil {
				skip(o, "unparsable")
				continue
			}
			opts = append(opts, asynq.Retention(d))
			applied = append(applied, o)
		case "MaxRetry":
			n, err := strconv.Atoi(arg)
			if err != nil {
				skip(o, "unparsable")
				continue
			}
			opts = append(opts, asynq.MaxRetry(n))
			applied = append(applied, o)
		case "Timeout":
			d, err := time.ParseDuration(arg)
			if err != nil {
				skip(o, "unparsable")
				continue
			}
			opts = append(opts, asynq.Timeout(d))
			applied = append(applied, o)
		case "Unique":
			d, err := time.ParseDuration(arg)
			if err != nil {
				skip(o, "unparsable")
				continue
			}
			opts = append(opts, asynq.Unique(d))
			applied = append(applied, o)
		case "Deadline":
			// asynq renders Deadline in time.UnixDate. Best-effort parse: an
			// unknown zone abbreviation would yield a zero-offset time, so
			// resolve against the local zone first (the common deployment:
			// scheduler and dashboard share a TZ) and honestly skip when the
			// string does not parse at all.
			t, err := time.ParseInLocation(time.UnixDate, arg, time.Local)
			if err != nil {
				skip(o, "unparsable")
				continue
			}
			opts = append(opts, asynq.Deadline(t))
			applied = append(applied, o)
		case "ProcessAt", "ProcessIn":
			// The whole point of run-now: the task is due immediately.
			skip(o, "schedule is replaced by run-now")
		case "TaskID":
			// The entry pins a fixed task id; reusing it would collide with
			// the scheduler's own enqueues. A fresh id is assigned.
			skip(o, "a fresh task id is assigned")
		case "Group":
			// Grouped tasks aggregate instead of running immediately, which
			// contradicts "run now" — and the aggregation window would sit on
			// the manual task indefinitely if no aggregator is configured.
			skip(o, "grouping is not applied on a manual run")
		default:
			skip(o, "unsupported option")
		}
	}
	return opts, applied, skipped
}

// splitOptString splits asynq's option rendering Name(arg) into (Name, arg).
// Quoted string args (Queue(%q) etc.) are unquoted.
func splitOptString(s string) (name, arg string, ok bool) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	name = s[:open]
	arg = s[open+1 : len(s)-1]
	if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
		if unq, err := strconv.Unquote(arg); err == nil {
			arg = unq
		}
	}
	return name, arg, true
}

// resolveSchedulerEntryData resolves a stable key to the entry's observed
// shape: live entries first (fresh payload/opts), then the persisted
// snapshot (GONE entries). found is false when neither knows the key.
func resolveSchedulerEntryData(inspector *asynq.Inspector, rc redis.UniversalClient, r *http.Request, stableKey string) (data stats.SchedulerEntryData, found bool, err error) {
	live, err := inspector.SchedulerEntries()
	if err != nil {
		return data, false, err
	}
	for _, le := range live {
		if stats.StableKeyForAsynqEntry(le) == stableKey {
			return stats.SchedulerEntryDataFromAsynq(le), true, nil
		}
	}
	snaps, err := stats.ReadSchedulerSnapshots(r.Context(), rc)
	if err != nil {
		return data, false, err
	}
	for _, s := range snaps {
		if s.StableKey == stableKey {
			return s.Entry, true, nil
		}
	}
	return data, false, nil
}

// newRunSchedulerEntryHandlerFunc serves POST /api/schedulers/{stable_key}/run
// (#337): resolve, reconstruct, enqueue through the shared §5.10 client,
// audit, and answer 201 with the created task plus the applied/skipped
// option ledger.
func newRunSchedulerEntryHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, client *asynq.Client, store *jobs.Store, pf PayloadFormatter, rf ResultFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stableKey := mux.Vars(r)["stable_key"]

		// The body is optional ({"reason": ...} only); an empty body is not
		// an error, a malformed one is.
		var req runSchedulerEntryRequest
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunSchedulerBodyBytes))
		if err != nil {
			writeErrorMsg(w, http.StatusBadRequest, "reading request body: "+err.Error())
			return
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}

		data, found, err := resolveSchedulerEntryData(inspector, rc, r, stableKey)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		if !found {
			writeErrorMsg(w, http.StatusNotFound, "no scheduler entry with this stable key")
			return
		}
		if data.TaskType == "" {
			// A snapshot without a task type cannot be reconstructed into a
			// task — refuse rather than enqueue a nameless task.
			writeErrorMsg(w, http.StatusUnprocessableEntity, "the entry snapshot has no task type — cannot reconstruct the task")
			return
		}

		taskOpts, applied, skipped := buildRunNowOptions(data)
		info, err := client.EnqueueContext(r.Context(), asynq.NewTask(data.TaskType, data.Payload), taskOpts...)
		if err != nil {
			switch {
			case errors.Is(err, asynq.ErrDuplicateTask):
				// The entry carries a Unique option and an identical task
				// holds the lock (very likely the scheduler's own recent
				// enqueue) — honest 409, mirroring the enqueue route.
				writeErrorMsg(w, http.StatusConflict,
					"a task with the same uniqueness lock already exists — the entry's Unique option blocks an identical manual run until the TTL expires")
			case errors.Is(err, asynq.ErrTaskIDConflict):
				writeErrorMsg(w, http.StatusConflict, "task ID conflicts with an existing task")
			default:
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}

		// Audit after the enqueue so the entry carries the created task id
		// (§5.11); reuses the §5.10 enqueue machinery with the #337 verb.
		// If the audit write fails the task still exists — say so.
		actor := actorFromContext(r.Context())
		if err := store.AppendAudit(r.Context(), jobs.AuditEntry{
			Event:  jobs.AuditTaskEnqueued,
			Actor:  actor.Name,
			Verb:   runSchedulerAuditVerb,
			Scope:  jobs.Scope{Queue: info.Queue, Q: info.Type},
			Reason: strings.TrimSpace(req.Reason),
			TaskID: info.ID,
			Acted:  1,
		}); err != nil {
			writeErrorMsg(w, http.StatusInternalServerError,
				fmt.Sprintf("task %s was enqueued, but the audit write failed: %v", info.ID, err))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(runSchedulerEntryResponse{
			Task:           toTaskInfo(info, pf, rf),
			AppliedOptions: applied,
			SkippedOptions: skipped,
		})
	}
}
