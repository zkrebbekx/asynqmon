package asynqmon

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for the Fleet Console stats endpoints (§5.1, §5.2):
//       GET /api/fleet/overview  — fleet aggregates + coverage stamp
//       GET /api/fleet/queues    — sorted/filtered page over cached snapshots
//       GET /api/fleet/attention — ranked attention findings (phase 4)
//   (GET /api/fleet/events, the SSE stream over the same payloads, lives in
//   fleet_events_handlers.go.)
//
// Both endpoints serve exclusively from the stats cache (in-process on the
// sweeping replica, shared Redis cache elsewhere) — never O(fleet) Redis
// reads per HTTP request. The pre-existing GET /api/queues (concurrent
// GetQueueInfo per queue) is left untouched for the shipping dashboard; the
// Queues Directory (phase 3) builds on /api/fleet/queues instead.
// ****************************************************************************

// coverageWindow is the freshness window the coverage stamp reports against.
// Fixed at 5m per the §3.1 contract ("totals cover N% of queues refreshed
// <5m") — in tier 1 a healthy sweeper keeps this at 100.
const coverageWindow = 5 * time.Minute

// fleetCoverage is the honest-numbers stamp attached to every stats
// response: how many queues the numbers cover, how fresh they are, which
// tier produced them, and where they were read from. The frontend contract
// depends on these fields being present from day one.
type fleetCoverage struct {
	// Tier of the stats engine that produced the data (§5.1 tiering table).
	// Always 1 until phase 12 lands rotating shards.
	Tier int `json:"tier"`
	// QueuesTotal is the number of queues in the cache.
	QueuesTotal int `json:"queues_total"`
	// RefreshedPct5m is the percentage (0-100) of queues whose snapshot is
	// younger than 5 minutes.
	RefreshedPct5m float64 `json:"refreshed_pct_5m"`
	// UpdatedAt is when the fleet aggregate was last swept (RFC3339).
	UpdatedAt string `json:"updated_at"`
	// Source is "local" (in-process, sweeping replica) or "cache" (shared
	// Redis cache).
	Source string `json:"source"`
}

type fleetAggregates struct {
	QueuesTotal        int `json:"queues_total"`
	PausedQueues       int `json:"paused_queues"`
	ZeroConsumerQueues int `json:"zero_consumer_queues"`

	Pending   int64 `json:"pending"`
	Active    int64 `json:"active"`
	Scheduled int64 `json:"scheduled"`
	Retry     int64 `json:"retry"`
	Archived  int64 `json:"archived"`
	Completed int64 `json:"completed"`
	Groups    int64 `json:"groups"`

	OrphanCandidates int64 `json:"orphan_candidates"`
	PastDueScheduled int64 `json:"past_due_scheduled"`

	ProcessedToday int64   `json:"processed_today"`
	FailedToday    int64   `json:"failed_today"`
	ErrorRate      float64 `json:"error_rate"`

	Servers      int `json:"servers"`
	WorkersTotal int `json:"workers_total"`
	WorkersBusy  int `json:"workers_busy"`

	// Redis memory gauge for the Fleet landing's Redis tile (§3.1): INFO
	// memory's used_memory / maxmemory, sampled once per sweep. Max 0 means
	// Redis has no configured limit; Used 0 means the INFO read failed
	// (unknown, not empty).
	RedisMemoryUsedBytes int64 `json:"redis_memory_used_bytes"`
	RedisMemoryMaxBytes  int64 `json:"redis_memory_max_bytes"`
}

type fleetOverviewResponse struct {
	Fleet    fleetAggregates `json:"fleet"`
	Coverage fleetCoverage   `json:"coverage"`
}

// fleetQueueRow is one row of the fleet queues directory. Every row carries
// refreshed_at so the UI can dot stale rows (§3.2 staleness honesty).
type fleetQueueRow struct {
	Queue string `json:"queue"`

	Pending   int64 `json:"pending"`
	Active    int64 `json:"active"`
	Scheduled int64 `json:"scheduled"`
	Retry     int64 `json:"retry"`
	Archived  int64 `json:"archived"`
	Completed int64 `json:"completed"`
	Groups    int64 `json:"groups"`

	Paused bool `json:"paused"`
	// PausedSince is RFC3339, empty when not paused (or when asynq's paused
	// marker held no parsable timestamp).
	PausedSince string `json:"paused_since,omitempty"`

	ProcessedToday int64   `json:"processed_today"`
	FailedToday    int64   `json:"failed_today"`
	ErrorRate      float64 `json:"error_rate"`

	OrphanCandidates int64 `json:"orphan_candidates"`
	// NextRetryAt is RFC3339, empty when the retry set is empty.
	NextRetryAt      string `json:"next_retry_at,omitempty"`
	PastDueScheduled int64  `json:"past_due_scheduled"`

	// OldestPendingSince is RFC3339, empty when no pending tasks (or age
	// unknown). OldestPendingAgeMs is the same datum as an age relative to
	// request time. LatencyMs is an alias of OldestPendingAgeMs — asynq's
	// queue "latency" IS the oldest pending age; both names are kept because
	// both appear as directory columns, but inventing two numbers would lie.
	OldestPendingSince string `json:"oldest_pending_since,omitempty"`
	OldestPendingAgeMs int64  `json:"oldest_pending_age_ms"`
	LatencyMs          int64  `json:"latency_ms"`

	Consumers int `json:"consumers"`

	// RefreshedAt is when this row's snapshot was swept (RFC3339).
	RefreshedAt string `json:"refreshed_at"`
}

func toFleetQueueRow(s *stats.QueueSnapshot, now time.Time) *fleetQueueRow {
	ageMs := s.OldestPendingAge(now).Milliseconds()
	return &fleetQueueRow{
		Queue:              s.Queue,
		Pending:            s.Pending,
		Active:             s.Active,
		Scheduled:          s.Scheduled,
		Retry:              s.Retry,
		Archived:           s.Archived,
		Completed:          s.Completed,
		Groups:             s.Groups,
		Paused:             s.Paused,
		PausedSince:        formatTimeInRFC3339(s.PausedSince),
		ProcessedToday:     s.ProcessedToday,
		FailedToday:        s.FailedToday,
		ErrorRate:          s.ErrorRate(),
		OrphanCandidates:   s.OrphanCandidates,
		NextRetryAt:        formatTimeInRFC3339(s.NextRetryAt),
		PastDueScheduled:   s.PastDueScheduled,
		OldestPendingSince: formatTimeInRFC3339(s.OldestPendingSince),
		OldestPendingAgeMs: ageMs,
		LatencyMs:          ageMs,
		Consumers:          s.Consumers,
		RefreshedAt:        formatTimeInRFC3339(s.RefreshedAt),
	}
}

// buildCoverage computes the coverage stamp over the FULL cached set — not
// the filtered subset — because it answers "how much of the fleet do these
// numbers see", not "how much matched".
func buildCoverage(res *stats.ReadResult, now time.Time) fleetCoverage {
	fresh := 0
	for _, s := range res.Queues {
		if now.Sub(s.RefreshedAt) <= coverageWindow {
			fresh++
		}
	}
	pct := 100.0
	if n := len(res.Queues); n > 0 {
		pct = float64(fresh) / float64(n) * 100
	}
	return fleetCoverage{
		Tier:           1, // phase 12 introduces tiers 2-3
		QueuesTotal:    len(res.Queues),
		RefreshedPct5m: pct,
		UpdatedAt:      formatTimeInRFC3339(res.Fleet.RefreshedAt),
		Source:         res.Source,
	}
}

// readStats centralizes the read + error translation shared by both
// handlers. A nil engine means stats were disabled via options.
func readStats(w http.ResponseWriter, r *http.Request, engine *stats.Engine) (*stats.ReadResult, bool) {
	if engine == nil {
		writeErrorMsg(w, http.StatusServiceUnavailable, "stats engine is disabled")
		return nil, false
	}
	res, err := engine.Read(r.Context())
	if err != nil {
		if errors.Is(err, stats.ErrNotReady) {
			writeErrorMsg(w, http.StatusServiceUnavailable,
				"fleet stats not available yet: the stats sweeper has not completed a sweep (it may be starting up, or no replica holds the sweeper lease)")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, err)
		return nil, false
	}
	return res, true
}

// buildFleetOverviewResponse converts a stats read into the overview
// response body. Shared by the HTTP handler and the SSE broker so the
// `overview` event payload is exactly the GET /api/fleet/overview JSON.
func buildFleetOverviewResponse(res *stats.ReadResult, now time.Time) fleetOverviewResponse {
	f := res.Fleet
	return fleetOverviewResponse{
		Fleet: fleetAggregates{
			QueuesTotal:          f.QueuesTotal,
			PausedQueues:         f.PausedQueues,
			ZeroConsumerQueues:   f.ZeroConsumerQueues,
			Pending:              f.Pending,
			Active:               f.Active,
			Scheduled:            f.Scheduled,
			Retry:                f.Retry,
			Archived:             f.Archived,
			Completed:            f.Completed,
			Groups:               f.Groups,
			OrphanCandidates:     f.OrphanCandidates,
			PastDueScheduled:     f.PastDueScheduled,
			ProcessedToday:       f.ProcessedToday,
			FailedToday:          f.FailedToday,
			ErrorRate:            f.ErrorRate(),
			Servers:              f.Servers,
			WorkersTotal:         f.WorkersTotal,
			WorkersBusy:          f.WorkersBusy,
			RedisMemoryUsedBytes: f.RedisMemoryUsed,
			RedisMemoryMaxBytes:  f.RedisMemoryMax,
		},
		Coverage: buildCoverage(res, now),
	}
}

// newFleetOverviewHandlerFunc serves GET /api/fleet/overview: fleet-wide
// aggregates plus the coverage stamp, entirely from the stats cache.
func newFleetOverviewHandlerFunc(engine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, ok := readStats(w, r, engine)
		if !ok {
			return
		}
		writeResponseJSON(w, buildFleetOverviewResponse(res, time.Now()))
	}
}

// newFleetAttentionHandlerFunc serves GET /api/fleet/attention (§5.2): the
// ranked instantaneous findings the sweep holder computed and published to
// the shared cache — every replica serves the identical report. The response
// body is the stats.AttentionReport JSON verbatim (frozen frontend
// contract); the SSE `attention` event carries the same bytes.
func newFleetAttentionHandlerFunc(engine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if engine == nil {
			writeErrorMsg(w, http.StatusServiceUnavailable, "stats engine is disabled")
			return
		}
		rep, err := engine.ReadAttention(r.Context())
		if err != nil {
			if errors.Is(err, stats.ErrNotReady) {
				writeErrorMsg(w, http.StatusServiceUnavailable,
					"fleet attention not available yet: the stats sweeper has not completed a sweep (it may be starting up, or no replica holds the sweeper lease)")
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeResponseJSON(w, rep)
	}
}

type fleetQueuesResponse struct {
	Queues       []*fleetQueueRow `json:"queues"`
	TotalMatched int              `json:"total_matched"`
	// NextCursor is the offset of the next page; omitted on the last page.
	NextCursor *int `json:"next_cursor,omitempty"`
	// FilterIgnored lists filter tokens the lenient v1 parser did not
	// understand — surfaced so a typo silently matching everything is
	// visible to the operator.
	FilterIgnored []string      `json:"filter_ignored,omitempty"`
	Coverage      fleetCoverage `json:"coverage"`
}

const (
	fleetQueuesDefaultLimit = 50
	fleetQueuesMaxLimit     = 500
)

// newFleetQueuesHandlerFunc serves GET /api/fleet/queues — the sorted,
// filtered, cursor-paginated directory page over cached snapshots.
//
// Query parameters:
//
//	sort   = name | pending | active | scheduled | retry | archived |
//	         completed | oldest_pending_age | latency | processed_today |
//	         failed_today | error_rate | consumers        (default: name)
//	dir    = asc | desc                                   (default: asc)
//	cursor = offset into the sorted+filtered result       (default: 0)
//	limit  = page size, 1..500                            (default: 50)
//	f      = filter, v1 grammar below
//
// The cursor is a plain offset: tier 1 sorts the full cached set in-process
// (≤~2k queues), so an offset is exact and cheap. When phase 12 introduces
// tiered caches the cursor becomes opaque; clients must already treat it so.
//
// Filter grammar v1 — NOT AQL. Phase 6 replaces this with the real parser
// (per-state gating, parse-time rejection, facets); keep this grammar frozen
// until then so saved URLs survive:
//
//	filter   := token (WS token)*          ; every token must match (AND)
//	token    := bareword                   ; substring match on queue name
//	          | "name~" text               ; substring match on queue name
//	          | "name=" text               ; exact queue name
//	          | "paused" ("="|"!=") bool   ; true/false
//	          | field op value             ; numeric comparison
//	field    := pending | active | scheduled | retry | archived | completed
//	          | groups | consumers | processed_today | failed_today
//	          | orphans | past_due | error_rate | oldest_pending_age | latency
//	op       := "=" | "!=" | ">" | ">=" | "<" | "<="
//	value    := number                     ; error_rate also accepts "N%"
//	          | duration                   ; age fields: 30m, 1h30m; bare = secs
//
// Parsing is lenient: unknown fields / unparsable values never fail the
// request; the offending tokens are echoed back in "filter_ignored".
func newFleetQueuesHandlerFunc(engine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		sortCol, err := stats.ParseSortColumn(q.Get("sort"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var desc bool
		switch q.Get("dir") {
		case "", "asc":
			desc = false
		case "desc":
			desc = true
		default:
			writeErrorMsg(w, http.StatusBadRequest, "dir must be \"asc\" or \"desc\"")
			return
		}
		cursor := 0
		if v := q.Get("cursor"); v != "" {
			cursor, err = strconv.Atoi(v)
			if err != nil || cursor < 0 {
				writeErrorMsg(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
		}
		limit := fleetQueuesDefaultLimit
		if v := q.Get("limit"); v != "" {
			limit, err = strconv.Atoi(v)
			if err != nil || limit < 1 || limit > fleetQueuesMaxLimit {
				writeErrorMsg(w, http.StatusBadRequest, "limit must be an integer between 1 and 500")
				return
			}
		}
		filter := stats.ParseFilter(q.Get("f"))

		res, ok := readStats(w, r, engine)
		if !ok {
			return
		}
		now := time.Now()

		matched := make([]*stats.QueueSnapshot, 0, len(res.Queues))
		for _, s := range res.Queues {
			if filter.Match(s, now) {
				matched = append(matched, s)
			}
		}
		stats.SortSnapshots(matched, sortCol, desc, now)

		start := cursor
		if start > len(matched) {
			start = len(matched)
		}
		end := start + limit
		if end > len(matched) {
			end = len(matched)
		}
		rows := make([]*fleetQueueRow, 0, end-start)
		for _, s := range matched[start:end] {
			rows = append(rows, toFleetQueueRow(s, now))
		}
		resp := fleetQueuesResponse{
			Queues:        rows,
			TotalMatched:  len(matched),
			FilterIgnored: filter.Ignored,
			Coverage:      buildCoverage(res, now),
		}
		if end < len(matched) {
			next := end
			resp.NextCursor = &next
		}
		writeResponseJSON(w, resp)
	}
}
