package asynqmon

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/hygiene"
	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// This file defines the Hygiene screen endpoints (build contract §3.10,
// phase 15):
//
//	GET  /api/hygiene               → report cards: config + last-run summary
//	GET  /api/hygiene/{kind}        → the full latest persisted report
//	POST /api/hygiene/{kind}/run    → generate NOW; returns the fresh report.
//	                                  Deliberately AVAILABLE IN READ-ONLY MODE
//	                                  (registered outside the read-only method
//	                                  filter): generation only reads asynq
//	                                  state and writes asynqmon-owned report
//	                                  keys. The trigger is audit-logged with
//	                                  the §5.11 actor either way.
//	PUT  /api/hygiene/{kind}/config → schedule config; read-only-blocked by
//	                                  the method filter, audit-logged.
//
// The JSON field names here (and in the hygiene package's report types) are
// FROZEN frontend contracts (ui/src/api-hygiene.ts). GET endpoints are plain
// store reads — any replica answers, scheduler running or not.
// ****************************************************************************

// hygieneCard is one row of the GET /api/hygiene response.
type hygieneCard struct {
	Kind            string `json:"kind"`
	Title           string `json:"title"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
	// LastGeneratedAt is RFC3339, "" when the report was never generated.
	LastGeneratedAt string `json:"last_generated_at"`
	// Summary is the last report's card chips; null when never generated.
	Summary map[string]int64 `json:"summary"`
}

type listHygieneResponse struct {
	Reports []hygieneCard `json:"reports"`
}

func newListHygieneHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		configs, err := hygiene.ReadConfigs(r.Context(), rc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		resp := listHygieneResponse{Reports: make([]hygieneCard, 0, len(hygiene.Kinds()))}
		for _, kind := range hygiene.Kinds() {
			cfg := configs[kind]
			card := hygieneCard{
				Kind:            kind,
				Title:           hygiene.Title(kind),
				Enabled:         cfg.Enabled,
				IntervalSeconds: cfg.IntervalSeconds,
			}
			rep, err := hygiene.ReadReport(r.Context(), rc, kind)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if rep != nil {
				card.LastGeneratedAt = rep.GeneratedAt.Format(time.RFC3339)
				card.Summary = rep.Summary
			}
			resp.Reports = append(resp.Reports, card)
		}
		writeResponseJSON(w, resp)
	}
}

func newGetHygieneReportHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := mux.Vars(r)["kind"]
		if !hygiene.ValidKind(kind) {
			writeErrorMsg(w, http.StatusBadRequest, "unknown hygiene report kind: "+kind)
			return
		}
		rep, err := hygiene.ReadReport(r.Context(), rc, kind)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if rep == nil {
			writeErrorMsg(w, http.StatusNotFound,
				"no "+kind+" report has been generated yet — use Run now or enable its schedule")
			return
		}
		writeResponseJSON(w, rep)
	}
}

// newRunHygieneHandlerFunc generates a report synchronously (generation is a
// bounded read burst — sub-second on tier-1 fleets) and returns it. The
// trigger is audit-logged BEFORE generation so an operator-induced load spike
// is attributable even if generation then fails.
func newRunHygieneHandlerFunc(engine *hygiene.Engine, store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := mux.Vars(r)["kind"]
		if !hygiene.ValidKind(kind) {
			writeErrorMsg(w, http.StatusBadRequest, "unknown hygiene report kind: "+kind)
			return
		}
		if engine == nil {
			writeErrorMsg(w, http.StatusServiceUnavailable, "hygiene engine is not running on this replica")
			return
		}
		actor := actorFromContext(r.Context())
		if err := store.AppendAudit(r.Context(), jobs.AuditEntry{
			Event:  jobs.AuditHygieneRun,
			Actor:  actor.Name,
			Verb:   kind,
			Reason: "manual run-now from the Hygiene screen",
		}); err != nil {
			// Audit is best-effort for a read-shaped trigger: log-and-continue
			// would hide a broken stream, so surface it but do not block runs.
			logHygieneAuditFailure(err)
		}
		rep, err := engine.Run(r.Context(), kind)
		if err != nil {
			if errors.Is(err, hygiene.ErrStatsUnavailable) {
				writeErrorMsg(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeResponseJSON(w, rep)
	}
}

// logHygieneAuditFailure is a seam so tests can assert we do not swallow
// audit failures silently in future changes; production just logs.
var logHygieneAuditFailure = func(err error) {
	log.Printf("asynqmon: hygiene: audit append failed: %v", err)
}

// putHygieneConfigRequest is the PUT body.
type putHygieneConfigRequest struct {
	Enabled         bool  `json:"enabled"`
	IntervalSeconds int64 `json:"interval_seconds"`
}

type putHygieneConfigResponse struct {
	Kind            string `json:"kind"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
}

func newPutHygieneConfigHandlerFunc(rc redis.UniversalClient, store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := mux.Vars(r)["kind"]
		if !hygiene.ValidKind(kind) {
			writeErrorMsg(w, http.StatusBadRequest, "unknown hygiene report kind: "+kind)
			return
		}
		var req putHygieneConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		cfg := hygiene.KindConfig{Enabled: req.Enabled, IntervalSeconds: req.IntervalSeconds}
		if err := hygiene.ValidateConfig(cfg); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := hygiene.WriteConfig(r.Context(), rc, kind, cfg); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		actor := actorFromContext(r.Context())
		reason := "disabled"
		if cfg.Enabled {
			reason = "enabled, every " + (time.Duration(cfg.IntervalSeconds) * time.Second).String()
		}
		if err := store.AppendAudit(r.Context(), jobs.AuditEntry{
			Event:  jobs.AuditHygieneConfig,
			Actor:  actor.Name,
			Verb:   kind,
			Reason: reason,
		}); err != nil {
			logHygieneAuditFailure(err)
		}
		writeResponseJSON(w, putHygieneConfigResponse{Kind: kind, Enabled: cfg.Enabled, IntervalSeconds: cfg.IntervalSeconds})
	}
}
