package asynqmon

import (
	"net/http"

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
		resp := listServersResponse{
			Servers: toServerInfoList(srvs, pf),
		}
		writeResponseJSON(w, resp)
	}
}
