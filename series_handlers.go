package asynqmon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines the §5.8 series read API (phase 10):
//
//	GET /api/series?scope=fleet|queue:<name>|server:<id>&metric=<m>
//	               &window=3h|24h|30d[&points=N]
//	  → {scope, metric, window, period, points:[{t,v|null}], learning_until?}
//
//	GET /api/series/batch?specs=<json array of the same fields>  (cap 60)
//	  → {series:[…same shape…]}   — one pipelined read for sparkline pages
//
// Points are period-aligned; v null = no-sample (a slot that was never
// flushed — NEVER rendered as zero). A series that was never written decodes
// to all-null points, not an error: absent data is absent, and the tier
// guard (§5.8) legitimately produces absent per-queue series on huge fleets.
//
// Handlers read exclusively from the shared ring-buffer strings any replica
// can serve; the lease-holding sweeper writes them (stats/series.go).
// ****************************************************************************

// seriesBatchCap bounds one batch request (contract: "cap 60 specs").
const seriesBatchCap = 60

// seriesReader is the injectable read dependency: production wires
// stats.ReadSeriesBatch over the shared Redis; handler unit tests inject a
// fake so parameter/shape handling is testable without a Redis.
type seriesReader func(ctx context.Context, specs []stats.SeriesSpec, now time.Time) ([]*stats.Series, error)

// newStatsSeriesReader is the production seriesReader.
func newStatsSeriesReader(rc redis.UniversalClient) seriesReader {
	return func(ctx context.Context, specs []stats.SeriesSpec, now time.Time) ([]*stats.Series, error) {
		return stats.ReadSeriesBatch(ctx, rc, specs, now)
	}
}

type seriesPointJSON struct {
	T int64  `json:"t"`
	V *int64 `json:"v"` // null = no-sample
}

type seriesJSON struct {
	Scope  string            `json:"scope"`
	Metric string            `json:"metric"`
	Window string            `json:"window"`
	Period int64             `json:"period"` // seconds per point
	Points []seriesPointJSON `json:"points"`
	// LearningUntil (RFC3339) is present while series sampling has existed
	// for less than 24h — ring-derived baselines are still learning.
	LearningUntil string `json:"learning_until,omitempty"`
}

func toSeriesJSON(s *stats.Series) seriesJSON {
	points := make([]seriesPointJSON, len(s.Points))
	for i, p := range s.Points {
		points[i] = seriesPointJSON{T: p.T, V: p.V}
	}
	return seriesJSON{
		Scope:         s.Scope.String(),
		Metric:        s.Metric,
		Window:        s.Window,
		Period:        s.Period,
		Points:        points,
		LearningUntil: formatTimeInRFC3339(s.LearningUntil),
	}
}

// parseSeriesSpec builds one stats.SeriesSpec from its wire fields.
func parseSeriesSpec(scope, metric, window, points string) (stats.SeriesSpec, string) {
	sc, err := stats.ParseSeriesScope(scope)
	if err != nil {
		return stats.SeriesSpec{}, err.Error()
	}
	spec := stats.SeriesSpec{Scope: sc, Metric: metric, Window: window}
	if points != "" {
		n, err := strconv.Atoi(points)
		if err != nil || n < 1 {
			return stats.SeriesSpec{}, "points must be a positive integer"
		}
		spec.MaxPoints = n
	}
	if err := stats.ValidateSeriesSpec(spec); err != nil {
		return stats.SeriesSpec{}, err.Error()
	}
	return spec, ""
}

// newGetSeriesHandlerFunc serves GET /api/series.
func newGetSeriesHandlerFunc(read seriesReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		spec, msg := parseSeriesSpec(q.Get("scope"), q.Get("metric"), q.Get("window"), q.Get("points"))
		if msg != "" {
			writeErrorMsg(w, http.StatusBadRequest, msg)
			return
		}
		out, err := read(r.Context(), []stats.SeriesSpec{spec}, time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeResponseJSON(w, toSeriesJSON(out[0]))
	}
}

// seriesBatchSpecJSON is one entry of the batch endpoint's specs parameter.
type seriesBatchSpecJSON struct {
	Scope  string `json:"scope"`
	Metric string `json:"metric"`
	Window string `json:"window"`
	Points int    `json:"points,omitempty"`
}

type seriesBatchResponse struct {
	Series []seriesJSON `json:"series"`
}

// newGetSeriesBatchHandlerFunc serves GET /api/series/batch — the sparkline
// pages' single round trip. Order of the response mirrors the specs order.
func newGetSeriesBatchHandlerFunc(read seriesReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("specs")
		if strings.TrimSpace(raw) == "" {
			writeErrorMsg(w, http.StatusBadRequest, "specs is required (JSON array of {scope, metric, window, points?})")
			return
		}
		var wire []seriesBatchSpecJSON
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, "specs must be a JSON array of {scope, metric, window, points?}")
			return
		}
		if len(wire) == 0 {
			writeErrorMsg(w, http.StatusBadRequest, "specs must contain at least one entry")
			return
		}
		if len(wire) > seriesBatchCap {
			writeErrorMsg(w, http.StatusBadRequest,
				"specs is capped at "+strconv.Itoa(seriesBatchCap)+" entries per request")
			return
		}
		specs := make([]stats.SeriesSpec, len(wire))
		for i, ws := range wire {
			points := ""
			if ws.Points != 0 {
				points = strconv.Itoa(ws.Points)
			}
			spec, msg := parseSeriesSpec(ws.Scope, ws.Metric, ws.Window, points)
			if msg != "" {
				writeErrorMsg(w, http.StatusBadRequest, "specs["+strconv.Itoa(i)+"]: "+msg)
				return
			}
			specs[i] = spec
		}
		out, err := read(r.Context(), specs, time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		resp := seriesBatchResponse{Series: make([]seriesJSON, len(out))}
		for i, s := range out {
			resp.Series[i] = toSeriesJSON(s)
		}
		writeResponseJSON(w, resp)
	}
}
