package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/zkrebbekx/asynqmon/jobs"
)

// ****************************************************************************
// This file defines the saved-view endpoints (build contract §4.2, phase 11):
//
//	GET    /api/views            → {views: […]}  (system first, then user
//	                                views newest-first)
//	POST   /api/views            {name, target, state} → 201 view
//	PUT    /api/views/{view_id}  {version, name?, target?, state?} → 200 view
//	DELETE /api/views/{view_id}  → 204
//
// A view is a shared team asset, so two operators can edit the same one.
// PUT must carry the `version` it read; a stale version answers 409
// {"error":"view changed"} and nothing is written. A name that another view
// already uses (compared without case) answers 409 on POST and on PUT.
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
	UpdatedAt string          `json:"updated_at"`
	Version   int             `json:"version"`
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
		UpdatedAt: formatTimeInRFC3339(v.UpdatedAt),
		Version:   normalizeViewVersion(v.Version),
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
	// Version is the version the client read. PUT requires it; POST ignores
	// it. A pointer so that a missing field is told apart from 0.
	Version *int `json:"version"`
}

// duplicateViewNameMsg is the 409 body for a name another view already uses.
func duplicateViewNameMsg(name string) string {
	return "a view named " + name + " already exists — pick another name"
}

// nameTakenByAnotherView reports whether another view already uses name.
// The comparison ignores case and surrounding space.
func nameTakenByAnotherView(ctx context.Context, store viewStore, name, excludeID string) (bool, error) {
	views, err := store.List(ctx)
	if err != nil {
		return false, err
	}
	_, found := findViewByName(views, name, excludeID)
	return found, nil
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
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
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
		// Fail closed: the cap protects the unpaginated List (ZRANGE 0 -1 +
		// HGETALL per id), so a Count error must not skip it.
		n, err := store.Count(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if n >= viewMaxCount {
			writeErrorMsg(w, http.StatusBadRequest, "view store is at its 500-view cap — delete stale views first")
			return
		}
		taken, err := nameTakenByAnotherView(r.Context(), store, name, "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if taken {
			writeErrorMsg(w, http.StatusConflict, duplicateViewNameMsg(name))
			return
		}
		now := time.Now()
		v := View{
			ID:        newViewID(),
			Name:      name,
			Target:    req.Target,
			State:     state,
			CreatedBy: viewActor(r),
			CreatedAt: now,
			UpdatedAt: now,
			Version:   1,
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
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, `invalid JSON body (want {"version", "name"?, "target"?, "state"?})`)
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
		if req.Version == nil {
			writeErrorMsg(w, http.StatusBadRequest,
				`version is required (send the "version" you read from GET /api/views)`)
			return
		}
		if normalizeViewVersion(*req.Version) != normalizeViewVersion(v.Version) {
			writeErrorMsg(w, http.StatusConflict, "view changed")
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
		if req.Name != "" {
			taken, err := nameTakenByAnotherView(r.Context(), store, v.Name, v.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if taken {
				writeErrorMsg(w, http.StatusConflict, duplicateViewNameMsg(v.Name))
				return
			}
		}
		v.Version = normalizeViewVersion(v.Version) + 1
		v.UpdatedAt = time.Now()
		ok, err = store.CompareAndPut(r.Context(), v, *req.Version)
		if err != nil {
			if errors.Is(err, errViewGone) {
				writeErrorMsg(w, http.StatusNotFound, "view not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			// Another writer bumped the version between the read and the
			// write: nothing was stored.
			writeErrorMsg(w, http.StatusConflict, "view changed")
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
