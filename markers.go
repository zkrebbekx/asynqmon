package asynqmon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// This file defines deploy markers (§5.14, phase 10):
//
//	POST /api/markers {label}        → 201 {id, label, actor, at}
//	GET  /api/markers?window=3h|24h|30d → {markers:[…]}  (ascending by time)
//
// Store: asynqmon:markers ZSET, score = Unix seconds, member = the marker's
// JSON. Markers are annotations rendered on the failure pulse and every
// chart; they are deliberately dumb shared state — no lease, any replica
// writes (§5.13 "deploy markers: plain shared Redis structures").
//
// The actor comes from the §5.11 identity resolution and every creation is
// audit-logged (the same capped asynqmon:audit stream the job runner uses).
// POST is blocked in read-only mode by the method filter in handler.go.
// ****************************************************************************

const (
	markersKey = "asynqmon:markers"

	// Retention: markers older than 90d (mirroring asynq's own 90d counter
	// horizon) are trimmed on write, and the set is hard-capped so a curl
	// loop cannot grow it unbounded.
	markerRetention = 90 * 24 * time.Hour
	markerMaxCount  = 1000

	markerMaxLabelLen = 200
)

// Marker is one deploy marker.
type Marker struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Actor string    `json:"actor"`
	At    time.Time `json:"-"`
}

// markerJSON is the wire shape (At as RFC3339).
type markerJSON struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Actor string `json:"actor"`
	At    string `json:"at"`
}

func toMarkerJSON(m Marker) markerJSON {
	return markerJSON{ID: m.ID, Label: m.Label, Actor: m.Actor, At: formatTimeInRFC3339(m.At)}
}

// markerStore abstracts the ZSET so handlers are unit-testable without
// Redis; redisMarkerStore is the production implementation.
type markerStore interface {
	Add(ctx context.Context, m Marker) error
	List(ctx context.Context, since time.Time) ([]Marker, error)
}

type redisMarkerStore struct {
	rc redis.UniversalClient
}

func newRedisMarkerStore(rc redis.UniversalClient) redisMarkerStore {
	return redisMarkerStore{rc: rc}
}

// storedMarker is the ZSET member payload. The timestamp rides inside the
// member as well as in the score so decoding needs no score juggling.
// Milliseconds, so two markers in the same second keep their real order.
type storedMarker struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Actor string `json:"actor"`
	At    int64  `json:"at"` // Unix milliseconds
}

func (s redisMarkerStore) Add(ctx context.Context, m Marker) error {
	payload, err := json.Marshal(storedMarker{ID: m.ID, Label: m.Label, Actor: m.Actor, At: m.At.UnixMilli()})
	if err != nil {
		return err
	}
	pipe := s.rc.Pipeline()
	pipe.ZAdd(ctx, markersKey, redis.Z{Score: float64(m.At.UnixMilli()) / 1000.0, Member: string(payload)})
	// Trim on write: drop the >90d tail, then cap the count (oldest first).
	pipe.ZRemRangeByScore(ctx, markersKey, "-inf",
		"("+formatUnix(m.At.Add(-markerRetention)))
	pipe.ZRemRangeByRank(ctx, markersKey, 0, int64(-markerMaxCount-1))
	_, err = pipe.Exec(ctx)
	return err
}

func (s redisMarkerStore) List(ctx context.Context, since time.Time) ([]Marker, error) {
	members, err := s.rc.ZRangeByScore(ctx, markersKey, &redis.ZRangeBy{
		Min: formatUnix(since),
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Marker, 0, len(members))
	for _, raw := range members {
		var sm storedMarker
		if err := json.Unmarshal([]byte(raw), &sm); err != nil {
			continue // a corrupt member is skipped, not fatal
		}
		out = append(out, Marker{ID: sm.ID, Label: sm.Label, Actor: sm.Actor, At: time.UnixMilli(sm.At)})
	}
	return out, nil
}

func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// ----------------------------------------------------------------------------
// Handlers.
// ----------------------------------------------------------------------------

// markerWindows maps the ?window= vocabulary (mirroring the series windows)
// to durations.
var markerWindows = map[string]time.Duration{
	"3h":  3 * time.Hour,
	"24h": 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

type createMarkerRequest struct {
	Label string `json:"label"`
}

// newCreateMarkerHandlerFunc serves POST /api/markers. The actor is the
// §5.11 resolved identity; creation is audit-logged.
func newCreateMarkerHandlerFunc(store markerStore, audit *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createMarkerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErrorMsg(w, http.StatusBadRequest, "invalid JSON body (want {\"label\": \"…\"})")
			return
		}
		label := strings.TrimSpace(req.Label)
		if label == "" {
			writeErrorMsg(w, http.StatusBadRequest, "label is required")
			return
		}
		if len(label) > markerMaxLabelLen {
			writeErrorMsg(w, http.StatusBadRequest, "label is capped at 200 characters")
			return
		}
		actor := actorFromContext(r.Context()).Name
		if actor == "" {
			// Middleware absent (embedded handler tests): stay honest rather
			// than inventing an identity.
			actor = "unattributed"
		}
		m := Marker{ID: newMarkerID(), Label: label, Actor: actor, At: time.Now()}
		if err := store.Add(r.Context(), m); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Audit (§5.11): markers are cheap but they annotate every chart —
		// who painted the line matters. Failure to audit fails the request
		// (consistent with job creation).
		if audit != nil {
			if err := audit.AppendAudit(r.Context(), jobs.AuditEntry{
				Event:  "marker_created",
				Actor:  m.Actor,
				Verb:   "marker",
				Reason: m.Label,
				JobID:  m.ID,
			}); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(toMarkerJSON(m))
	}
}

type listMarkersResponse struct {
	Markers []markerJSON `json:"markers"`
}

// newListMarkersHandlerFunc serves GET /api/markers?window=3h|24h|30d
// (default 24h), ascending by time.
func newListMarkersHandlerFunc(store markerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		window := r.URL.Query().Get("window")
		if window == "" {
			window = "24h"
		}
		d, ok := markerWindows[window]
		if !ok {
			writeErrorMsg(w, http.StatusBadRequest, "invalid window (want 3h, 24h or 30d)")
			return
		}
		markers, err := store.List(r.Context(), time.Now().Add(-d))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		resp := listMarkersResponse{Markers: make([]markerJSON, 0, len(markers))}
		for _, m := range markers {
			resp.Markers = append(resp.Markers, toMarkerJSON(m))
		}
		writeResponseJSON(w, resp)
	}
}

// newMarkerID returns a short random hex id (8 bytes / 16 chars).
func newMarkerID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
