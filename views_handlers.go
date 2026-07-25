package asynqmon

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// This file defines the saved-view endpoints (build contract §4.2, phase 11):
//
//	GET    /api/views            → {views: […]}  (system first, then user
//	                                views newest-first)
//	POST   /api/views            {name, target, state} → 201 view
//	PUT    /api/views/{view_id}  {name?, target?, state?} → 200 view
//	DELETE /api/views/{view_id}  → 204
//
// Mutations are actor-attributed (§5.11 middleware) and audit-logged on the
// same capped asynqmon:audit stream the job runner uses. Read-only mode
// blocks all three mutations via the method filter in handler.go. System
// views answer 400 with the reason on PUT/DELETE — they are shipped product,
// not user data.
// ****************************************************************************

// viewJSON is the wire shape of one view.
type viewJSON struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Target    string          `json:"target"`
	State     json.RawMessage `json:"state"`
	CreatedBy string          `json:"created_by"`
	CreatedAt string          `json:"created_at"`
	System    bool            `json:"system"`
}

func toViewJSON(v View) viewJSON {
	return viewJSON{
		ID:        v.ID,
		Name:      v.Name,
		Target:    v.Target,
		State:     v.State,
		CreatedBy: v.CreatedBy,
		CreatedAt: formatTimeInRFC3339(v.CreatedAt),
		System:    v.System,
	}
}

type listViewsResponse struct {
	Views []viewJSON `json:"views"`
}

// newListViewsHandlerFunc serves GET /api/views.
func newListViewsHandlerFunc(store viewStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		views, err := store.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		sortViewsForDisplay(views)
		resp := listViewsResponse{Views: make([]viewJSON, 0, len(views))}
		for _, v := range views {
			resp.Views = append(resp.Views, toViewJSON(v))
		}
		writeResponseJSON(w, resp)
	}
}

type upsertViewRequest struct {
	Name   string          `json:"name"`
	Target string          `json:"target"`
	State  json.RawMessage `json:"state"`
}

// validateViewName trims and bounds the name; returns ("", reason) on error.
func validateViewName(raw string) (string, string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "name is required"
	}
	if len(name) > viewMaxNameLen {
		return "", "name is capped at 120 characters"
	}
	return name, ""
}

func validateViewTarget(target string) string {
	if target != viewTargetTasks && target != viewTargetQueues {
		return `target must be "tasks" or "queues"`
	}
	return ""
}

// viewActor resolves the acting user for attribution, mirroring the markers
// handler: middleware absent (embedded handler tests) stays honest as
// "unattributed" rather than inventing an identity.
func viewActor(r *http.Request) string {
	if a := actorFromContext(r.Context()).Name; a != "" {
		return a
	}
	return "unattributed"
}

// auditView appends one view mutation to the §5.11 audit stream. Failure to
// audit fails the request (consistent with job creation and markers).
func auditView(w http.ResponseWriter, r *http.Request, audit *jobs.Store, event string, v View) bool {
	if audit == nil {
		return true
	}
	if err := audit.AppendAudit(r.Context(), jobs.AuditEntry{
		Event:  event,
		Actor:  viewActor(r),
		Verb:   "view",
		Reason: v.Name,
		JobID:  v.ID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	return true
}

// newCreateViewHandlerFunc serves POST /api/views.
func newCreateViewHandlerFunc(store viewStore, audit *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req upsertViewRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, `invalid JSON body (want {"name", "target", "state"})`)
			return
		}
		name, reason := validateViewName(req.Name)
		if reason != "" {
			writeErrorMsg(w, http.StatusBadRequest, reason)
			return
		}
		if reason := validateViewTarget(req.Target); reason != "" {
			writeErrorMsg(w, http.StatusBadRequest, reason)
			return
		}
		state, reason := validateViewState(req.State)
		if reason != "" {
			writeErrorMsg(w, http.StatusBadRequest, reason)
			return
		}
		if n, err := store.Count(r.Context()); err == nil && n >= viewMaxCount {
			writeErrorMsg(w, http.StatusBadRequest, "view store is at its 500-view cap — delete stale views first")
			return
		}
		v := View{
			ID:        newViewID(),
			Name:      name,
			Target:    req.Target,
			State:     state,
			CreatedBy: viewActor(r),
			CreatedAt: time.Now(),
			System:    false,
		}
		if err := store.Put(r.Context(), v); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !auditView(w, r, audit, "view_created", v) {
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(toViewJSON(v))
	}
}

// newUpdateViewHandlerFunc serves PUT /api/views/{view_id}. Partial updates:
// absent fields keep their stored values; created_by/created_at never change.
func newUpdateViewHandlerFunc(store viewStore, audit *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["view_id"]
		var req upsertViewRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, `invalid JSON body (want {"name"?, "target"?, "state"?})`)
			return
		}
		v, ok, err := store.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErrorMsg(w, http.StatusNotFound, "view not found")
			return
		}
		if v.System {
			writeErrorMsg(w, http.StatusBadRequest,
				"system views cannot be modified — "+v.Name+" is maintained by asynqmon; save a copy as a new view instead")
			return
		}
		if req.Name != "" {
			name, reason := validateViewName(req.Name)
			if reason != "" {
				writeErrorMsg(w, http.StatusBadRequest, reason)
				return
			}
			v.Name = name
		}
		if req.Target != "" {
			if reason := validateViewTarget(req.Target); reason != "" {
				writeErrorMsg(w, http.StatusBadRequest, reason)
				return
			}
			v.Target = req.Target
		}
		if len(req.State) > 0 {
			state, reason := validateViewState(req.State)
			if reason != "" {
				writeErrorMsg(w, http.StatusBadRequest, reason)
				return
			}
			v.State = state
		}
		if err := store.Put(r.Context(), v); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !auditView(w, r, audit, "view_updated", v) {
			return
		}
		writeResponseJSON(w, toViewJSON(v))
	}
}

// newDeleteViewHandlerFunc serves DELETE /api/views/{view_id}. System views
// are undeletable — 400 with the reason (§4.2).
func newDeleteViewHandlerFunc(store viewStore, audit *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["view_id"]
		v, ok, err := store.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErrorMsg(w, http.StatusNotFound, "view not found")
			return
		}
		if v.System {
			writeErrorMsg(w, http.StatusBadRequest,
				"system views are undeletable — "+v.Name+" ships with asynqmon and is re-seeded at startup")
			return
		}
		if err := store.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !auditView(w, r, audit, "view_deleted", v) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
