package asynqmon

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for the Schedulers screen (build contract §3.8/§5.12):
//       GET /api/schedulers                        — live ∪ snapshot rows,
//         merged on the stable entry key so history survives scheduler
//         restarts and a dead scheduler renders as SCHEDULER GONE
//       GET /api/schedulers/{stable_key}/outcomes  — the entry's enqueue
//         events joined to current task state: succeeded / failed / pending /
//         unknown(not_found — task deleted or Retention=0)
//   - the legacy /api/scheduler_entries endpoints stay untouched
//     (scheduler_entry_handlers.go) for back-compat.
// ****************************************************************************

const (
	// schedulerHistoryCap is asynq's hard cap on per-entry enqueue-event
	// history: RecordSchedulerEnqueueEvent LTRIMs the event list to 1,000
	// (github.com/hibiken/asynq@v0.24.1/internal/rdb/rdb.go, maxEvents).
	// Carried in the outcomes response so the UI labels the cap honestly.
	schedulerHistoryCap = 1000

	// maxOutcomeEvents bounds the outcome join: the newest N events get a
	// GetTaskInfo each (N sequential reads per request).
	maxOutcomeEvents = 50

	// outcomeReasonNotFound is the honest three-o'clock explanation for a
	// joined task that no longer exists (§3.8 Retention=0 outcome gap).
	outcomeReasonNotFound = "not_found — task deleted or Retention=0"
)

// schedulerRow is one merged row of GET /api/schedulers. Frozen frontend
// contract — additive changes only.
type schedulerRow struct {
	StableKey string          `json:"stable_key"`
	Entry     *schedulerEntry `json:"entry"`
	Live      bool            `json:"live"`
	// FirstSeen/LastSeen come from the sweeper's snapshot; empty when the
	// entry is live but no sweep has persisted it yet (absent data stays
	// absent, never fabricated as "now").
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	// GoneSince (== last_seen) is present only when the entry is GONE: no
	// live counterpart and last_seen older than the gone threshold.
	GoneSince string `json:"gone_since,omitempty"`
}

type listSchedulersResponse struct {
	Entries []*schedulerRow `json:"entries"`
}

// snapshotSchedulerEntry renders a persisted observation in the exact JSON
// shape of a live entry (conversion_helpers.go), formatting the stored raw
// payload through the same PayloadFormatter.
func snapshotSchedulerEntry(d stats.SchedulerEntryData, pf PayloadFormatter) *schedulerEntry {
	opts := d.Opts
	if opts == nil {
		opts = make([]string, 0)
	}
	prev := ""
	if !d.Prev.IsZero() {
		prev = d.Prev.Format(time.RFC3339)
	}
	return &schedulerEntry{
		ID:            d.EntryID,
		Spec:          d.Spec,
		TaskType:      d.TaskType,
		TaskPayload:   pf.FormatPayload(d.TaskType, d.Payload),
		Opts:          opts,
		NextEnqueueAt: d.Next.Format(time.RFC3339),
		PrevEnqueueAt: prev,
	}
}

func newListSchedulersHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, goneAfter time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		live, err := inspector.SchedulerEntries()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		snaps, err := stats.ReadSchedulerSnapshots(r.Context(), rc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		snapByKey := make(map[string]*stats.SchedulerSnapshot, len(snaps))
		for _, s := range snaps {
			snapByKey[s.StableKey] = s
		}

		now := time.Now()
		rows := make([]*schedulerRow, 0, len(live)+len(snaps))
		seen := make(map[string]bool, len(live))
		for _, le := range live {
			key := stats.StableKeyForAsynqEntry(le)
			if seen[key] {
				continue // two schedulers heartbeating identical entries
			}
			seen[key] = true
			row := &schedulerRow{StableKey: key, Entry: toSchedulerEntry(le, pf), Live: true}
			if s, ok := snapByKey[key]; ok {
				row.FirstSeen = rfc3339OrEmpty(s.FirstSeen)
				row.LastSeen = rfc3339OrEmpty(s.LastSeen)
			}
			rows = append(rows, row)
		}
		for _, s := range snaps {
			if seen[s.StableKey] {
				continue
			}
			row := &schedulerRow{
				StableKey: s.StableKey,
				Entry:     snapshotSchedulerEntry(s.Entry, pf),
				Live:      false,
				FirstSeen: rfc3339OrEmpty(s.FirstSeen),
				LastSeen:  rfc3339OrEmpty(s.LastSeen),
			}
			// GONE only past the threshold: a snapshot that merely lags one
			// heartbeat (scheduler restarting, entry re-registering) renders
			// as a plain non-live row, not a finding.
			if !s.LastSeen.IsZero() && now.Sub(s.LastSeen) > goneAfter {
				row.GoneSince = rfc3339OrEmpty(s.LastSeen)
			}
			rows = append(rows, row)
		}

		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Entry.TaskType != rows[j].Entry.TaskType {
				return rows[i].Entry.TaskType < rows[j].Entry.TaskType
			}
			if rows[i].Entry.Spec != rows[j].Entry.Spec {
				return rows[i].Entry.Spec < rows[j].Entry.Spec
			}
			return rows[i].StableKey < rows[j].StableKey
		})
		writeResponseJSON(w, listSchedulersResponse{Entries: rows})
	}
}

// schedulerOutcomeEvent is one joined enqueue event of the outcomes response.
type schedulerOutcomeEvent struct {
	TaskID     string `json:"task_id"`
	EnqueuedAt string `json:"enqueued_at"`
	// Outcome is three-valued plus pending: "succeeded" (completed-state task
	// found), "failed" (archived, or retry-state with retries > 0), "pending"
	// (still pending/active/scheduled), "unknown" (task no longer in Redis).
	Outcome string `json:"outcome"`
	// State is the raw asynq task state when the task was found.
	State string `json:"state,omitempty"`
	// Reason spells out why an outcome is unknown.
	Reason string `json:"reason,omitempty"`
}

type schedulerOutcomesResponse struct {
	StableKey string `json:"stable_key"`
	Queue     string `json:"queue"`
	// RetentionSeconds is the entry's asynq.Retention in whole seconds; 0
	// means completions are deleted immediately and success is unknowable —
	// the UI renders the §3.8 guidance off this field.
	RetentionSeconds int64 `json:"retention_seconds"`
	// HistoryCap labels asynq's 1,000-event enqueue-history cap.
	HistoryCap int                      `json:"history_cap"`
	Events     []*schedulerOutcomeEvent `json:"events"`
}

// schedulerOutcome maps a found task's state to the three-valued outcome
// (§3.8): completed → succeeded; archived or retry-with-retries → failed;
// everything still in flight → pending.
func schedulerOutcome(state asynq.TaskState, retried int) string {
	switch state {
	case asynq.TaskStateCompleted:
		return "succeeded"
	case asynq.TaskStateArchived:
		return "failed"
	case asynq.TaskStateRetry:
		if retried > 0 {
			return "failed"
		}
		return "pending"
	default: // pending, active, scheduled, aggregating
		return "pending"
	}
}

func newSchedulerOutcomesHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stableKey := mux.Vars(r)["stable_key"]

		// Resolve the stable key against live entries first (fresh queue /
		// retention / entry ID), then the snapshot (GONE entries; also the
		// entry-ID history that survives scheduler restarts).
		live, err := inspector.SchedulerEntries()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		var liveEntry *asynq.SchedulerEntry
		for _, le := range live {
			if stats.StableKeyForAsynqEntry(le) == stableKey {
				liveEntry = le
				break
			}
		}
		snaps, err := stats.ReadSchedulerSnapshots(r.Context(), rc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		var snap *stats.SchedulerSnapshot
		for _, s := range snaps {
			if s.StableKey == stableKey {
				snap = s
				break
			}
		}
		if liveEntry == nil && snap == nil {
			writeErrorMsg(w, http.StatusNotFound, "no scheduler entry with this stable key")
			return
		}

		var data stats.SchedulerEntryData
		var entryIDs []string
		if liveEntry != nil {
			data = stats.SchedulerEntryDataFromAsynq(liveEntry)
			entryIDs = []string{liveEntry.ID}
		}
		if snap != nil {
			if liveEntry == nil {
				data = snap.Entry
			}
			for _, id := range snap.EntryIDs {
				if len(entryIDs) == 0 || entryIDs[0] != id {
					entryIDs = append(entryIDs, id)
				}
			}
		}

		// Merge enqueue events across the observed entry IDs (asynq keys
		// history by the ephemeral ID; the ID list is the §5.12 re-keying),
		// newest first, capped. A missing history list is an empty slice, not
		// an error, so gracefully-shutdown schedulers (asynq clears their
		// history) yield an empty trace.
		var events []*asynq.SchedulerEnqueueEvent
		for _, id := range entryIDs {
			evs, err := inspector.ListSchedulerEnqueueEvents(id, asynq.PageSize(maxOutcomeEvents))
			if err != nil {
				writeError(w, errorStatus(err), err)
				return
			}
			events = append(events, evs...)
		}
		sort.Slice(events, func(i, j int) bool { return events[i].EnqueuedAt.After(events[j].EnqueuedAt) })
		if len(events) > maxOutcomeEvents {
			events = events[:maxOutcomeEvents]
		}

		out := make([]*schedulerOutcomeEvent, 0, len(events))
		for _, ev := range events {
			joined := &schedulerOutcomeEvent{
				TaskID:     ev.TaskID,
				EnqueuedAt: ev.EnqueuedAt.Format(time.RFC3339),
			}
			ti, err := inspector.GetTaskInfo(data.Queue, ev.TaskID)
			switch {
			case err == nil:
				joined.State = ti.State.String()
				joined.Outcome = schedulerOutcome(ti.State, ti.Retried)
			case errors.Is(err, asynq.ErrTaskNotFound), errors.Is(err, asynq.ErrQueueNotFound):
				joined.Outcome = "unknown"
				joined.Reason = outcomeReasonNotFound
			default:
				writeError(w, errorStatus(err), err)
				return
			}
			out = append(out, joined)
		}

		writeResponseJSON(w, schedulerOutcomesResponse{
			StableKey:        stableKey,
			Queue:            data.Queue,
			RetentionSeconds: data.RetentionSec,
			HistoryCap:       schedulerHistoryCap,
			Events:           out,
		})
	}
}

// rfc3339OrEmpty formats t, or returns "" for the zero time (JSON-friendly
// "absent", matching prev_enqueue_at's convention).
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
