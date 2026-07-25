package asynqmon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// This file defines the HTTP surface of the flag-gated enqueue capability
// (build contract §5.10) plus capability discovery:
//
//   POST /api/queues/{qname}/tasks   embedded asynq.Client enqueue
//   GET  /api/features               {"features":{"enqueue":bool}}
//
// Gating: Options.EnableEnqueue (default false, --enable-enqueue) AND not
// ReadOnly. When gated off, the route answers 403 JSON with the reason —
// registered outside the read-only method filter so the answer is never a
// bare text 405 (see the §5.10 route block in handler.go).
//
// Every successful enqueue writes an audit entry (§5.11): verb "enqueue",
// scope = queue + type, count 1, plus the created task id. Actor comes from
// the identity chain; --require-identity refuses anonymous mutations before
// this handler runs.
//
// Validation and option mapping live in enqueue.go.
// ****************************************************************************

// enqueueAuditVerb is the audit verb for §5.10 entries.
const enqueueAuditVerb = "enqueue"

// newEnqueueTaskHandlerFunc serves POST /api/queues/{qname}/tasks: validate,
// enqueue through the embedded client, audit, and answer 201 with the created
// task in the same taskInfo shape GET /api/queues/{qname}/tasks/{task_id}
// returns (converted via the configured formatters).
func newEnqueueTaskHandlerFunc(client *asynq.Client, store *jobs.Store, pf PayloadFormatter, rf ResultFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qname := mux.Vars(r)["qname"]
		r.Body = http.MaxBytesReader(w, r.Body, maxEnqueueBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req enqueueTaskRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		taskType, payload, taskOpts, errMsg := buildEnqueueTask(qname, &req, time.Now())
		if errMsg != "" {
			writeErrorMsg(w, http.StatusBadRequest, errMsg)
			return
		}

		info, err := client.EnqueueContext(r.Context(), asynq.NewTask(taskType, payload), taskOpts...)
		if err != nil {
			switch {
			case errors.Is(err, asynq.ErrDuplicateTask):
				// Unique option conflict: an identical task holds the
				// uniqueness lock until its TTL expires (§3.5 drawer copy).
				writeErrorMsg(w, http.StatusConflict,
					"a task with the same uniqueness lock already exists — enqueue of an identical task is blocked until the unique TTL expires")
			case errors.Is(err, asynq.ErrTaskIDConflict):
				writeErrorMsg(w, http.StatusConflict, "task ID conflicts with an existing task")
			default:
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}

		// Audit after the enqueue so the entry can carry the created task id
		// (§5.11). If the audit write fails the task still exists — say so
		// rather than pretending the whole operation failed.
		actor := actorFromContext(r.Context())
		if err := store.AppendAudit(r.Context(), jobs.AuditEntry{
			Event:  jobs.AuditTaskEnqueued,
			Actor:  actor.Name,
			Verb:   enqueueAuditVerb,
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
		json.NewEncoder(w).Encode(toTaskInfo(info, pf, rf))
	}
}

// newEnqueueDisabledHandlerFunc answers the enqueue route when the capability
// is gated off: 403 JSON with the reason (§5.10), never a bare 405.
func newEnqueueDisabledHandlerFunc(readOnly bool) http.HandlerFunc {
	msg := "enqueue is disabled: start asynqmon with --enable-enqueue (env ENABLE_ENQUEUE=true) to allow creating tasks from the UI"
	if readOnly {
		msg = "enqueue is disabled: asynqmon is running in read-only mode"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		writeErrorMsg(w, http.StatusForbidden, msg)
	}
}

// featuresResponse is the wire shape of GET /api/features. Field names are a
// frontend contract (ui/src/api.ts FeaturesResponse).
type featuresResponse struct {
	Features struct {
		Enqueue bool `json:"enqueue"`
	} `json:"features"`
}

// newFeaturesHandlerFunc serves GET /api/features: the capability flags the
// frontend gates UI on (the clone-and-edit action hides entirely when
// enqueue is off). Deliberately not part of /api/fleet/overview — that
// endpoint 503s whenever the stats engine is disabled or has not swept yet,
// and capability discovery must not depend on it.
func newFeaturesHandlerFunc(enqueueEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var resp featuresResponse
		resp.Features.Enqueue = enqueueEnabled
		writeResponseJSON(w, resp)
	}
}
