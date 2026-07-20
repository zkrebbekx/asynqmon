package asynqmon

import (
	"net/http"
	"sort"

	"github.com/gorilla/mux"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for scheduler entry related endpoints
// ****************************************************************************

func newListSchedulerEntriesHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := inspector.SchedulerEntries()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		// Entries are stored in a sorted set scored by heartbeat expiry, so the
		// order changes on every scheduler heartbeat. Sort by ID for stability.
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		payload := make(map[string]interface{})
		if len(entries) == 0 {
			// avoid nil for the entries field in json output.
			payload["entries"] = make([]*schedulerEntry, 0)
		} else {
			payload["entries"] = toSchedulerEntries(entries, pf)
		}
		writeResponseJSON(w, payload)
	}
}

type listSchedulerEnqueueEventsResponse struct {
	Events []*schedulerEnqueueEvent `json:"events"`
}

func newListSchedulerEnqueueEventsHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entryID := mux.Vars(r)["entry_id"]
		pageSize, pageNum := getPageOptions(r)
		events, err := inspector.ListSchedulerEnqueueEvents(
			entryID, asynq.PageSize(pageSize), asynq.Page(pageNum))
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		resp := listSchedulerEnqueueEventsResponse{
			Events: toSchedulerEnqueueEvents(events),
		}
		writeResponseJSON(w, resp)
	}
}
