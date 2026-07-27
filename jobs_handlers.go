package asynqmon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// This file defines the bulk-job + audit HTTP surface (build contract §3.9,
// §4.3, §5.4, §5.11):
//
//   POST /api/jobs                  create a job (always phase=preview)
//   GET  /api/jobs?limit=           list jobs, newest first
//   GET  /api/jobs/{id}             full job incl. sample + failure page
//   GET  /api/jobs/{id}/results     page of the job's own enumerated matches
//   POST /api/jobs/{id}/execute     {proceed_on_partial} — gated execute
//   POST /api/jobs/{id}/cancel
//   POST /api/jobs/{id}/pause
//   POST /api/jobs/{id}/resume
//   GET  /api/audit?limit=          audit stream, newest first
//
// Handlers are store-backed: any replica serves them; the leased runner
// (started in New) claims and performs the work. Mutating routes are POSTs,
// so read-only mode's method filter blocks them like every other mutation.
// ****************************************************************************

// jobJSON is the frozen wire shape of a job. Field names are a frontend
// contract (ui/src/api.ts JobInfo). The struct is canonically defined as
// jobs.WireJob so the /api/jobs bodies and the asynqmon:events:jobs pub/sub
// payloads (jobs/events.go, fed into the SSE `jobs` event) can never drift.
type jobJSON = jobs.WireJob

func toJobJSON(j *jobs.Job) jobJSON {
	return jobs.WireOf(j)
}

// jobThrottles are the accepted tasks/sec settings (§4.3 confirm step). The
// zero value falls back to the default.
var jobThrottles = map[int]bool{100: true, 200: true, 500: true, 1000: true}

const defaultJobThrottle = 200

type createJobRequest struct {
	Verb     string     `json:"verb"`
	Scope    jobs.Scope `json:"scope"`
	Reason   string     `json:"reason"`
	Throttle int        `json:"throttle"`
	Phase    string     `json:"phase"`
}

// newCreateJobHandlerFunc validates and persists a new preview-phase job.
// The claiming runner picks it up within its poll interval.
func newCreateJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req createJobRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Phase != "" && req.Phase != string(jobs.PhasePreview) {
			// Build-contract §4.3; the reference stays out of the user-facing text.
			writeErrorMsg(w, http.StatusBadRequest, "jobs are always created in the preview phase; execute is a separate, gated request")
			return
		}
		verb := jobs.Verb(req.Verb)
		switch verb {
		case jobs.VerbRun, jobs.VerbArchive, jobs.VerbDelete, jobs.VerbCancel:
		default:
			writeErrorMsg(w, http.StatusBadRequest, "invalid verb (want run|archive|delete|cancel)")
			return
		}
		if reason := jobs.ValidateScopeVerb(req.Scope, verb); reason != "" {
			writeErrorMsg(w, http.StatusBadRequest, reason)
			return
		}
		// AQL scopes (phase 6) are validated at create time: parse + state
		// gating + the env-free matcher requirement. Rejections use the
		// structured {error, position, hint} shape the console renders.
		if perr := req.Scope.ValidateAql(); perr != nil {
			writeAqlError(w, perr)
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			// Reason strings are mandatory regardless of identity mode (§5.11).
			writeErrorMsg(w, http.StatusBadRequest, "reason is required — it goes to the audit log")
			return
		}
		throttle := req.Throttle
		if throttle == 0 {
			throttle = defaultJobThrottle
		}
		if !jobThrottles[throttle] {
			writeErrorMsg(w, http.StatusBadRequest, "invalid throttle (want 100|200|500|1000 tasks/sec)")
			return
		}

		actor := actorFromContext(r.Context())
		j := &jobs.Job{
			ID:        jobs.NewJobID(),
			Verb:      verb,
			Scope:     req.Scope,
			Phase:     jobs.PhasePreview,
			State:     jobs.StatePreviewing,
			Throttle:  throttle,
			Reason:    strings.TrimSpace(req.Reason),
			Actor:     actor.Name,
			CostClass: jobs.ComputeCostClass(req.Scope, verb),
			CreatedAt: time.Now(),
		}
		if err := store.Create(r.Context(), j); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Audit at job create (§5.11); completion is audited by the runner.
		if err := store.AppendAudit(r.Context(), jobs.AuditEntry{
			Event:  jobs.AuditJobCreated,
			Actor:  j.Actor,
			Verb:   string(j.Verb),
			Scope:  j.Scope,
			Reason: j.Reason,
			JobID:  j.ID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(toJobJSON(j))
	}
}

type listJobsResponse struct {
	Jobs []jobJSON `json:"jobs"`
}

func newListJobsHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := atoiDefault(r.URL.Query().Get("limit"), 50)
		if limit < 1 || limit > 200 {
			limit = 50
		}
		list, err := store.List(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out := make([]jobJSON, 0, len(list))
		for _, j := range list {
			out = append(out, toJobJSON(j))
		}
		writeResponseJSON(w, listJobsResponse{Jobs: out})
	}
}

type getJobResponse struct {
	jobJSON
	Sample        []jobs.SampleRow   `json:"sample"`
	Failures      []jobs.ItemFailure `json:"failures"`
	FailuresTotal int64              `json:"failures_total"` // recorded (capped) count; overflow is separate
}

func newGetJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["job_id"]
		j, err := store.Get(r.Context(), id)
		if err != nil {
			writeJobError(w, err)
			return
		}
		q := r.URL.Query()
		offset := int64(atoiDefault(q.Get("failures_offset"), 0))
		limit := int64(atoiDefault(q.Get("failures_limit"), 100))
		if limit < 1 || limit > 1000 {
			limit = 100
		}
		sample, err := store.Sample(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		failures, failuresTotal, err := store.Failures(r.Context(), id, offset, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if sample == nil {
			sample = []jobs.SampleRow{}
		}
		if failures == nil {
			failures = []jobs.ItemFailure{}
		}
		writeResponseJSON(w, getJobResponse{
			jobJSON:       toJobJSON(j),
			Sample:        sample,
			Failures:      failures,
			FailuresTotal: failuresTotal,
		})
	}
}

// jobResultsResponse is one page of a job's own enumerated result set.
// Rows are searchTask-shaped — the same shape /api/tasks serves — so the
// console's results table renders them unchanged.
type jobResultsResponse struct {
	Tasks           []*searchTask `json:"tasks"`
	TotalCandidates int64         `json:"total_candidates"`
	Offset          int64         `json:"offset"`
	// Vanished counts refs on THIS page whose task no longer exists
	// (deleted/moved since the scan) — skipped and stated, never an error.
	Vanished int `json:"vanished"`
}

const (
	defaultJobResultsLimit = 50
	maxJobResultsLimit     = 200
)

// newJobResultsHandlerFunc serves pages of a job's persisted candidate list,
// hydrated to live task rows. This is the console's source of truth after a
// scan-to-completion job finishes: the job already enumerated the EXACT
// result set — re-running the query as a fresh budgeted scan would drop
// matches beyond the first budget window. Candidate refs are not capped in
// the store (only the 10-row sample and the 10k failure report are), so
// total_candidates is the job's full match count. Read-only; any replica.
func newJobResultsHandlerFunc(store *jobs.Store, insp *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["job_id"]
		if _, err := store.Get(r.Context(), id); err != nil {
			writeJobError(w, err) // 404 for unknown jobs
			return
		}
		q := r.URL.Query()
		offset := int64(atoiDefault(q.Get("offset"), 0))
		if offset < 0 {
			offset = 0
		}
		limit := int64(atoiDefault(q.Get("limit"), defaultJobResultsLimit))
		if limit < 1 || limit > maxJobResultsLimit {
			limit = defaultJobResultsLimit
		}
		total, err := store.CandidateCount(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		refs, err := store.Candidates(r.Context(), id, offset, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Hydrate the page with the same bounded-concurrency, order-preserving
		// fan-out the cursor listing uses (aql_exec.go). A nil slot means the
		// task vanished since enumeration — skipped, honestly counted.
		infos := fetchTaskInfos(insp, refs)
		tasks := make([]*searchTask, 0, len(infos))
		vanished := 0
		for _, ti := range infos {
			if ti == nil {
				vanished++
				continue
			}
			tasks = append(tasks, toSearchTask(ti, pf))
		}
		writeResponseJSON(w, jobResultsResponse{
			Tasks:           tasks,
			TotalCandidates: total,
			Offset:          offset,
			Vanished:        vanished,
		})
	}
}

type executeJobRequest struct {
	ProceedOnPartial bool `json:"proceed_on_partial"`
	// Throttle optionally overrides the job's throttle at execute time
	// (0 = keep the value set at creation).
	Throttle int `json:"throttle"`
	// Reason optionally replaces the job's reason at execute time — the
	// confirm modal starts the preview before its reason field is filled
	// in ("" = keep). The completion audit entry carries this final value.
	Reason string `json:"reason"`
}

func newExecuteJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["job_id"]
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		var req executeJobRequest
		// An absent or malformed body degrades to proceed_on_partial=false —
		// the safe direction (the gate then requires a completed preview).
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Throttle != 0 && !jobThrottles[req.Throttle] {
			writeErrorMsg(w, http.StatusBadRequest, "invalid throttle (want 100|200|500|1000 tasks/sec)")
			return
		}
		j, err := store.RequestExecute(r.Context(), id, req.ProceedOnPartial, req.Throttle, strings.TrimSpace(req.Reason))
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeResponseJSON(w, toJobJSON(j))
	}
}

func newCancelJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, err := store.RequestCancel(r.Context(), mux.Vars(r)["job_id"])
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeResponseJSON(w, toJobJSON(j))
	}
}

func newPauseJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, err := store.RequestPause(r.Context(), mux.Vars(r)["job_id"])
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeResponseJSON(w, toJobJSON(j))
	}
}

func newResumeJobHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, err := store.RequestResume(r.Context(), mux.Vars(r)["job_id"])
		if err != nil {
			writeJobError(w, err)
			return
		}
		writeResponseJSON(w, toJobJSON(j))
	}
}

// writeJobError maps store errors onto HTTP status codes: 404 unknown job,
// 409 wrong state, 412 unmet execute gate.
func writeJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, jobs.ErrWrongState):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, jobs.ErrPreviewIncomplete), errors.Is(err, jobs.ErrPartialNotAcked):
		writeError(w, http.StatusPreconditionFailed, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

type listAuditResponse struct {
	Entries []jobs.AuditEntry `json:"entries"`
}

// newListAuditHandlerFunc serves the capped audit stream, newest first (§5.11).
func newListAuditHandlerFunc(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := int64(atoiDefault(r.URL.Query().Get("limit"), 100))
		if limit < 1 || limit > 1000 {
			limit = 100
		}
		entries, err := store.ReadAudit(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if entries == nil {
			entries = []jobs.AuditEntry{}
		}
		writeResponseJSON(w, listAuditResponse{Entries: entries})
	}
}
