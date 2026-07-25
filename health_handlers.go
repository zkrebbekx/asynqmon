package asynqmon

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/errsig"
	"github.com/hibiken/asynqmon/hygiene"
	"github.com/hibiken/asynqmon/internal/leasefence"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - GET /healthz — orchestrator liveness/readiness probe (upstream
//     hibiken/asynqmon#276): PING redis with a 2s budget; 200 or 503.
//   - GET /api/health/roles — the Settings › Health readout (build contract
//     §3.12, phase 12): per singleton role (§5.13) the lease holder instance,
//     TTL and fencing counter; for the stats engine additionally the §5.1
//     tier status, command budget, coverage, hot-set size, rotation estimate,
//     series-sampler status, and this replica's last local sweep cost.
//
// Role leases are read straight from the shared leasefence keys, so any
// replica serves the same holder truth. The last-sweep cost block is
// HOLDER-LOCAL (in-process measurement) and labeled so: on a standby replica
// it reports the last sweep THIS replica ran (usually none), never a
// fabricated remote number.
//
// The bulk-job runner is deliberately absent from the roles list: it is not
// a process singleton — execution is claimed per job with its own fencing
// token (§5.13), visible per job on the Operations screen.
// ****************************************************************************

// healthzResponse is the GET /healthz probe body (upstream
// hibiken/asynqmon#276).
type healthzResponse struct {
	Status string `json:"status"`          // "ok" | "unavailable"
	Error  string `json:"error,omitempty"` // redis error when unavailable
}

// newHealthzHandlerFunc serves GET /healthz (upstream hibiken/asynqmon#276):
// a liveness/readiness probe that PINGs redis with a 2-second budget so a
// hung Redis cannot stall the orchestrator's probe loop. 200 {"status":"ok"}
// when redis answers, 503 {"status":"unavailable","error":...} otherwise.
// Registered on the parent router (outside /api): no auth, no read-only
// filter, no side effects.
func newHealthzHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := rc.Ping(ctx).Err(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(healthzResponse{Status: "unavailable", Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(healthzResponse{Status: "ok"})
	}
}

// healthRole is one §5.13 singleton role's lease state.
type healthRole struct {
	Role  string `json:"role"`
	Title string `json:"title"`
	// Holder is the lease value (instance ID); "" when unheld (role down or
	// between handovers).
	Holder string `json:"holder"`
	// HeldByThisReplica is true when this serving replica owns the lease.
	// Null-ish (false) when this replica cannot know (engine disabled here).
	HeldByThisReplica bool `json:"held_by_this_replica"`
	// TTLMs is the lease's remaining life in milliseconds (0 = unheld).
	TTLMs int64 `json:"ttl_ms"`
	// Fence is the role's fencing counter — how many claims ever happened;
	// stale-token writers below this value are rejected (§5.13).
	Fence int64 `json:"fence"`
}

// healthSweep is the holder-local last-sweep cost.
type healthSweep struct {
	At         string `json:"at"` // RFC3339
	Queues     int    `json:"queues"`
	Refreshed  int    `json:"refreshed"`
	ReadCmds   int    `json:"read_cmds"`
	WriteCmds  int    `json:"write_cmds"`
	DurationMs int64  `json:"duration_ms"`
}

// healthSeries describes the §5.8 sampler's current mode.
type healthSeries struct {
	// Mode: "full" (tier 1 — every queue, both rings), "hot+rotation"
	// (tiers 2-3 — hot set both rings, cold rollup-only on rotation visits),
	// "hot-only" (fleet above the hard ceiling).
	Mode string `json:"mode"`
	// QueueCeiling is the hard §5.8 safety ceiling.
	QueueCeiling int `json:"queue_ceiling"`
}

type healthStats struct {
	// Available is false when the stats engine is disabled on this replica
	// or no sweep has ever published — the other fields are then absent.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	Tier                   int     `json:"tier,omitempty"`
	CommandBudget          int     `json:"command_budget,omitempty"`
	HotSetSize             int     `json:"hot_set_size,omitempty"`
	FullRotationSecondsEst int     `json:"full_rotation_seconds_est,omitempty"`
	QueuesTotal            int     `json:"queues_total,omitempty"`
	CoveragePct5m          float64 `json:"coverage_pct_5m,omitempty"`
	UpdatedAt              string  `json:"updated_at,omitempty"`
	Source                 string  `json:"source,omitempty"`

	// LastSweepLocal is the last sweep THIS replica ran (holder-local
	// measurement; absent on replicas that never held the sweeper lease).
	LastSweepLocal *healthSweep  `json:"last_sweep_local,omitempty"`
	Series         *healthSeries `json:"series,omitempty"`
}

type healthRolesResponse struct {
	Roles     []healthRole `json:"roles"`
	Stats     healthStats  `json:"stats"`
	UpdatedAt string       `json:"updated_at"` // RFC3339, request time
}

// roleSpec joins a leasefence role with its display title and this replica's
// instance identity (empty when the engine is absent here).
type roleSpec struct {
	role, title, localInstance string
}

// newHealthRolesHandlerFunc serves GET /api/health/roles (§3.12).
func newHealthRolesHandlerFunc(rc redis.UniversalClient, engine *stats.Engine, indexer *errsig.Indexer, hygieneEng *hygiene.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		now := time.Now()

		specs := []roleSpec{
			{role: "stats", title: "Stats sweeper"},
			{role: "errsig", title: "Error-signature indexer"},
			{role: "hygiene", title: "Hygiene scheduler"},
		}
		if engine != nil {
			specs[0].localInstance = engine.InstanceID()
		}
		if indexer != nil {
			specs[1].localInstance = indexer.InstanceID()
		}
		if hygieneEng != nil {
			specs[2].localInstance = hygieneEng.InstanceID()
		}

		roles := make([]healthRole, 0, len(specs))
		for _, spec := range specs {
			info, err := leasefence.New(rc, spec.role).Holder(ctx)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			roles = append(roles, healthRole{
				Role:              spec.role,
				Title:             spec.title,
				Holder:            info.Holder,
				HeldByThisReplica: info.Holder != "" && info.Holder == spec.localInstance,
				TTLMs:             info.TTL.Milliseconds(),
				Fence:             info.Fence,
			})
		}

		st := healthStats{}
		if engine == nil {
			st.Reason = "stats engine is disabled on this replica"
		} else if res, err := engine.Read(ctx); err != nil {
			st.Reason = "no sweep has published yet (sweeper starting up, or no replica holds the lease)"
		} else {
			cov := buildCoverage(res, now)
			st.Available = true
			st.Tier = cov.Tier
			st.CommandBudget = engine.CommandBudget()
			st.HotSetSize = cov.HotSetSize
			st.FullRotationSecondsEst = cov.FullRotationSecondsEst
			st.QueuesTotal = cov.QueuesTotal
			st.CoveragePct5m = cov.RefreshedPct5m
			st.UpdatedAt = cov.UpdatedAt
			st.Source = cov.Source

			ceiling := engine.SeriesQueueCeilingValue()
			mode := "full"
			switch {
			case cov.QueuesTotal > ceiling:
				mode = "hot-only"
			case cov.Tier > 1:
				mode = "hot+rotation"
			}
			st.Series = &healthSeries{Mode: mode, QueueCeiling: ceiling}

			if sweep := engine.LastSweep(); !sweep.At.IsZero() {
				st.LastSweepLocal = &healthSweep{
					At:         formatTimeInRFC3339(sweep.At),
					Queues:     sweep.Queues,
					Refreshed:  sweep.Refreshed,
					ReadCmds:   sweep.ReadCmds,
					WriteCmds:  sweep.WriteCmds,
					DurationMs: sweep.Duration.Milliseconds(),
				}
			}
		}

		writeResponseJSON(w, healthRolesResponse{
			Roles:     roles,
			Stats:     st,
			UpdatedAt: formatTimeInRFC3339(now),
		})
	}
}
