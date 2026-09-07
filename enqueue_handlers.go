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

	"github.com/zkrebbekx/asynqmon/jobs"
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
func newEnqueueTaskHandlerFunc(client *asynq.Client, inspector *asynq.Inspector, store *jobs.Store, pf PayloadFormatter, rf ResultFormatter) http.HandlerFunc {
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
		if errMsg := checkEnqueueQueueExists(inspector, qname, req.CreateQueue); errMsg != "" {
			writeErrorMsg(w, http.StatusBadRequest, errMsg)
			return
		}

		// asynq 0.26 carries a header map on the task message. NewTask
		// creates a task WITHOUT headers, so a clone of a task that has them
		// must be built with NewTaskWithHeaders or the metadata is dropped.
		task := asynq.NewTask(taskType, payload)
		if len(req.Headers) > 0 {
			task = asynq.NewTaskWithHeaders(taskType, payload, req.Headers)
		}
		info, err := client.EnqueueContext(r.Context(), task, taskOpts...)
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

// checkEnqueueQueueExists refuses an unknown queue name unless the body asked
// for the queue to be created. A typed queue name would otherwise create a
// zero-consumer queue that the inventory hygiene report then flags (§3.10).
// It returns "" when the request may proceed. A failed Queues() read also
// returns "": the enqueue itself is the authority, and a Redis hiccup must
// not block a legitimate task.
func checkEnqueueQueueExists(inspector *asynq.Inspector, qname string, createQueue bool) string {
	if createQueue || inspector == nil {
		return ""
	}
	qnames, err := inspector.Queues()
	if err != nil {
		return ""
	}
	for _, q := range qnames {
		if q == qname {
			return ""
		}
	}
	return fmt.Sprintf("queue: %q does not exist — check the name, "+
		`or resend with "create_queue": true to create it`, qname)
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
	// CorrelationKeys is the configured payload-key list the task drawer's
	// Flow view recognizes as correlation ids (§3.5), in priority order.
	// Older frontends ignore it; older backends omit it and the frontend
	// falls back to its built-in default list.
	CorrelationKeys []string `json:"correlation_keys"`
	// PayloadDetailLimit is the character cap the task DETAIL endpoint's
	// formatters apply (--max-detail-payload-length, upstream
	// hibiken/asynqmon#301); 0 = unlimited/unknown. The drawer uses it to
	// render an honest "truncated at N chars" note on capped payloads.
	PayloadDetailLimit int `json:"payload_detail_limit"`
	// Version is the build version of the serving binary (Options.Version);
	// "" when the embedder did not set it.
	Version string `json:"version"`
}

// defaultCorrelationKeys is the Flow view's correlation-key list when
// Options.CorrelationKeys is unset (--correlation-keys default). Mirrored by
// the frontend fallback (ui/src/lib/correlation.ts DEFAULT_CORRELATION_KEYS).
var defaultCorrelationKeys = []string{"trace_id", "correlation_id", "request_id"}

// normalizeCorrelationKeys trims entries, drops empties, and removes
// duplicates (first occurrence keeps its priority slot). An empty result
// falls back to defaultCorrelationKeys, so a stray "--correlation-keys ,,"
// can never turn the Flow view off by accident.
func normalizeCorrelationKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	if len(out) == 0 {
		return defaultCorrelationKeys
	}
	return out
}

// newFeaturesHandlerFunc serves GET /api/features: the capability flags the
// frontend gates UI on (the clone-and-edit action hides entirely when
// enqueue is off) plus the correlation-key list the Flow view honors.
// Deliberately not part of /api/fleet/overview — that endpoint 503s whenever
// the stats engine is disabled or has not swept yet, and capability
// discovery must not depend on it.
func newFeaturesHandlerFunc(enqueueEnabled bool, correlationKeys []string, payloadDetailLimit int, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var resp featuresResponse
		resp.Features.Enqueue = enqueueEnabled
		resp.CorrelationKeys = correlationKeys
		resp.PayloadDetailLimit = payloadDetailLimit
		resp.Version = version
		writeResponseJSON(w, resp)
	}
}
