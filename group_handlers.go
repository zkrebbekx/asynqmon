package asynqmon

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/hibiken/asynq"
)

type listGroupsResponse struct {
	Queue  *queueStateSnapshot `json:"stats"`
	Groups []*groupInfo        `json:"groups"`
}

func newListGroupsHandlerFunc(inspector *asynq.Inspector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qname := mux.Vars(r)["qname"]

		groups, err := inspector.Groups(qname)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		qinfo, err := inspector.GetQueueInfo(qname)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		resp := listGroupsResponse{
			Queue:  toQueueStateSnapshot(qinfo),
			Groups: toGroupInfos(groups),
		}
		writeResponseJSON(w, resp)
	}
}
