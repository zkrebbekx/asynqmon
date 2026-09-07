package asynqmon

import (
	"errors"
	"net/http"
	"sort"
	"sync"

	"github.com/gorilla/mux"

	"github.com/hibiken/asynq"

	"github.com/zkrebbekx/asynqmon/internal/safego"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for queue related endpoints
// ****************************************************************************

// queueInfoConcurrency bounds per-request fan-out on the legacy queue
// endpoints (GetQueueInfo / History per queue) so a large fleet cannot turn
// one dashboard poll into thousands of simultaneous Redis command bursts.
const queueInfoConcurrency = 16

func newListQueuesHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qnames, err := inspector.Queues()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		// Queues() is backed by a Redis SMEMBERS, whose order is unspecified and
		// varies between calls. Sort so the dashboard rows and charts keep a
		// stable order across polls instead of reshuffling.
		sort.Strings(qnames)
		// GetQueueInfo does several redis calls (incl. MEMORY USAGE sampling) per
		// queue; fetch queues concurrently so the homepage stays fast with many
		// queues — but bounded: unbounded fan-out meant one homepage poll on a
		// 2000-queue fleet fired 2000 simultaneous multi-command bursts.
		snapshots := make([]*queueStateSnapshot, len(qnames))
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			firstErr error
		)
		sem := make(chan struct{}, queueInfoConcurrency)
		for i, qname := range qnames {
			wg.Add(1)
			// Go 1.22 loop variables are per-iteration, so the closure
			// captures this i and qname.
			safego.Go("queues: queue-info fan-out", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
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
			})
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
		// History is ~numdays reads per queue; run queues concurrently but
		// bounded (this endpoint used to issue O(queues × 90) reads strictly
		// serially, and unbounded fan-out would be the opposite mistake).
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			firstErr error
		)
		sem := make(chan struct{}, queueInfoConcurrency)
		for _, qname := range qnames {
			wg.Add(1)
			safego.Go("queues: history fan-out", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				stats, err := inspector.History(qname, numdays)
				if err != nil {
					if errors.Is(err, asynq.ErrQueueNotFound) {
						return // queue deleted mid-request
					}
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				resp.Stats[qname] = toDailyStatsList(stats)
				mu.Unlock()
			})
		}
		wg.Wait()
		if firstErr != nil {
			writeError(w, errorStatus(firstErr), firstErr)
			return
		}
		writeResponseJSON(w, resp)
	}
}
