package asynqmon

import (
	"net/http"
	"sort"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for server related endpoints
// ****************************************************************************

type listServersResponse struct {
	Servers []*serverInfo `json:"servers"`
}

func newListServersHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srvs, err := inspector.Servers()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		// Servers are read from a sorted set scored by heartbeat expiry, so
		// every heartbeat reshuffles the returned order. Sort by identity to
		// keep the table rows stable across polls.
		sort.Slice(srvs, func(i, j int) bool {
			if srvs[i].Host != srvs[j].Host {
				return srvs[i].Host < srvs[j].Host
			}
			return srvs[i].PID < srvs[j].PID
		})
		resp := listServersResponse{
			Servers: toServerInfoList(srvs, pf),
		}
		writeResponseJSON(w, resp)
	}
}
