package asynqmon

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// This file defines http.Handler(s) for the Workers screen (§3.7):
//   GET /api/coverage         — queues × consuming-servers join (§5.5)
//   GET /api/cancel-listeners — cancel deliverability via PUBSUB NUMSUB (§5.14)
// Join logic lives in coverage.go.
// ****************************************************************************

// asynqCancelChannel is asynq's cancelation pub/sub channel. Asynq does not
// export the name; it is base.CancelChannel = "asynq:cancel" in asynq
// v0.24.1, internal/base/base.go line 40 ("PubSub channel"). Every running
// asynq.Server subscribes to it (rdb.CancelationPubSub), so NUMSUB on it
// counts the servers that can actually hear a cancel signal.
const asynqCancelChannel = "asynq:cancel"

type coverageResponse struct {
	Rows []*coverageRow `json:"rows"`
	// UpdatedAt is when this join was computed (RFC3339). The join reads
	// live Servers()/Queues() per request — only the pending column comes
	// from the stats cache and may be older.
	UpdatedAt string `json:"updated_at"`
}

// newCoverageHandlerFunc serves GET /api/coverage. Live inspector reads
// (O(servers + queues), both single commands + heartbeat hash reads) joined
// with the stats cache for pending depths. A missing or not-ready stats
// engine degrades the response — pending omitted, depth-dependent flags
// unflagged — instead of failing it: "who consumes Z" must answer even when
// the sweeper is down.
func newCoverageHandlerFunc(inspector *asynq.Inspector, engine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srvs, err := inspector.Servers()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		queues, err := inspector.Queues()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		var depths map[string]coverageDepth
		if engine != nil {
			if res, err := engine.Read(r.Context()); err == nil {
				depths = make(map[string]coverageDepth, len(res.Queues))
				for _, s := range res.Queues {
					depths[s.Queue] = coverageDepth{Pending: s.Pending, Active: s.Active}
				}
			}
		}
		writeResponseJSON(w, coverageResponse{
			Rows:      buildCoverageRows(srvs, queues, depths),
			UpdatedAt: time.Now().Format(time.RFC3339),
		})
	}
}

type cancelListenersResponse struct {
	// Listeners is the number of subscribers on asynq's cancelation channel
	// — effectively the number of asynq servers that would receive a cancel
	// signal.
	Listeners int64 `json:"listeners"`
	// Approximate is true on Redis Cluster: NUMSUB reports per-node counts,
	// so the number is a sum over the nodes that answered (§3.7 caveat).
	Approximate bool `json:"approximate"`
	// Nodes is how many Redis nodes contributed to the sum (always 1 off
	// cluster) — the UI's "summed across N reachable nodes" figure.
	Nodes int `json:"nodes"`
}

// newCancelListenersHandlerFunc serves GET /api/cancel-listeners: PUBSUB
// NUMSUB on asynq's cancelation channel. On Redis Cluster subscriptions live
// on whichever node each subscriber happened to connect to, so the count is
// summed across every reachable node and labeled approximate; nodes that do
// not answer are skipped (a partially reachable cluster yields an honest
// lower bound rather than an error).
func newCancelListenersHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		switch c := rc.(type) {
		case *redis.ClusterClient:
			var (
				mu    sync.Mutex
				total int64
				nodes int
			)
			// ForEachShard visits masters and replicas; clients may hold
			// subscriptions on either. Per-node errors are swallowed on
			// purpose — see the lower-bound note above.
			_ = c.ForEachShard(ctx, func(ctx context.Context, shard *redis.Client) error {
				counts, err := shard.PubSubNumSub(ctx, asynqCancelChannel).Result()
				if err != nil {
					return nil
				}
				mu.Lock()
				total += counts[asynqCancelChannel]
				nodes++
				mu.Unlock()
				return nil
			})
			if nodes == 0 {
				writeErrorMsg(w, http.StatusInternalServerError,
					"no cluster node answered PUBSUB NUMSUB")
				return
			}
			writeResponseJSON(w, cancelListenersResponse{Listeners: total, Approximate: true, Nodes: nodes})
		default:
			counts, err := rc.PubSubNumSub(ctx, asynqCancelChannel).Result()
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeResponseJSON(w, cancelListenersResponse{Listeners: counts[asynqCancelChannel], Approximate: false, Nodes: 1})
		}
	}
}
