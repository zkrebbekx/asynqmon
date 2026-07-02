package asynqmon

import (
	"errors"
	"net/http"
	"sync"

	"github.com/gorilla/mux"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for queue related endpoints
// ****************************************************************************

func newListQueuesHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qnames, err := inspector.Queues()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		// GetQueueInfo does several redis calls (incl. MEMORY USAGE sampling) per
		// queue; fetch queues concurrently so the homepage stays fast with many queues.
		snapshots := make([]*queueStateSnapshot, len(qnames))
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			firstErr error
		)
		for i, qname := range qnames {
			wg.Add(1)
			go func(i int, qname string) {
				defer wg.Done()
				qinfo, err := inspector.GetQueueInfo(qname)
				if err != nil {
					// A queue deleted mid-request is not an error for the listing.
					if !errors.Is(err, asynq.ErrQueueNotFound) {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
					}
					return
				}
				snapshots[i] = toQueueStateSnapshot(qinfo)
			}(i, qname)
		}
		wg.Wait()
		if firstErr != nil {
			writeError(w, errorStatus(firstErr), firstErr)
			return
		}
		queues := make([]*queueStateSnapshot, 0, len(snapshots))
		for _, s := range snapshots {
			if s != nil {
				queues = append(queues, s)
			}
		}
		writeResponseJSON(w, map[string]interface{}{"queues": queues})
	}
}

func newGetQueueHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		qname := vars["qname"]

		payload := make(map[string]interface{})
		qinfo, err := inspector.GetQueueInfo(qname)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		payload["current"] = toQueueStateSnapshot(qinfo)

		// TODO: make this n a variable
		data, err := inspector.History(qname, 10)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		// avoid null for the history field in json output.
		dailyStats := make([]*dailyStats, 0, len(data))
		for _, s := range data {
			dailyStats = append(dailyStats, toDailyStats(s))
		}
		payload["history"] = dailyStats
		writeResponseJSON(w, payload)
	}
}

func newDeleteQueueHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		qname := vars["qname"]
		if err := inspector.DeleteQueue(qname, false); err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func newPauseQueueHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		qname := vars["qname"]
		if err := inspector.PauseQueue(qname); err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func newResumeQueueHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		qname := vars["qname"]
		if err := inspector.UnpauseQueue(qname); err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type listQueueStatsResponse struct {
	Stats map[string][]*dailyStats `json:"stats"`
}

func newListQueueStatsHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qnames, err := inspector.Queues()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		resp := listQueueStatsResponse{Stats: make(map[string][]*dailyStats)}
		const numdays = 90 // Get stats for the last 90 days.
		for _, qname := range qnames {
			stats, err := inspector.History(qname, numdays)
			if err != nil {
				if errors.Is(err, asynq.ErrQueueNotFound) {
					continue // queue deleted mid-request
				}
				writeError(w, errorStatus(err), err)
				return
			}
			resp.Stats[qname] = toDailyStatsList(stats)
		}
		writeResponseJSON(w, resp)
	}
}
