package asynqmon

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

// ****************************************************************************
// This file defines:
//   - http.Handler(s) for the Queue Workspace right-rail histograms
//     (build contract §3.3(c)):
//       GET /api/queues/{qname}/retry_histogram     — retry-ETA buckets
//       GET /api/queues/{qname}/pending_wait_sample — pending-wait sampling
//
// Honesty rules (§3.3): the retry histogram is EXACT — pipelined ZCOUNT
// windows over the retry zset's next_process_at scores (O(log N) each).
// The pending-wait sample is NOT uniform and says so: pending is a Redis
// LIST, LINDEX costs O(index) per probe, so probes are drawn from the head
// and tail bands only and the response labels itself
// "sampled, head/tail-biased" — never claimed representative.
// ****************************************************************************

// The retry zset and pending list key formats are asynq internals mirrored
// locally (same convention as task_timing.go, version-pinned to
// asynq@v0.24.1/internal/base). A miss degrades to zeros, never an error.

func retryZSetKey(qname string) string {
	return fmt.Sprintf("asynq:{%s}:retry", qname)
}

func pendingListKey(qname string) string {
	return fmt.Sprintf("asynq:{%s}:pending", qname)
}

// clampInt parses raw as an int, falling back to def and clamping into
// [min, max].
func clampInt(raw string, def, min, max int) int {
	n := def
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		}
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// ----------------------------------------------------------------------------
// GET /api/queues/{qname}/retry_histogram
// ----------------------------------------------------------------------------

const (
	defaultRetryWindowSeconds = 1800 // 30 minutes, per the mockup's "next 30m"
	defaultRetryBuckets       = 12
)

type retryHistogramResponse struct {
	Queue string `json:"queue"`
	// From is the request time — bucket i covers
	// [from + i*bucket_seconds, from + (i+1)*bucket_seconds).
	From          string `json:"from"`
	BucketSeconds int    `json:"bucket_seconds"`
	// PastDue counts retry tasks whose next_process_at is already behind
	// now (the recoverer/forwarder will fire them imminently).
	PastDue int64   `json:"past_due"`
	Buckets []int64 `json:"buckets"`
	// Beyond counts tasks firing after the last bucket's end.
	Beyond int64 `json:"beyond"`
	Total  int64 `json:"total"`
	// Exact is always true: these are ZCOUNTs on the retry zset scores,
	// not samples. (The pending-wait endpoint is the sampled one.)
	Exact bool `json:"exact"`
}

// newRetryHistogramHandlerFunc answers "when does the retry storm land"
// with exact pipelined ZCOUNT windows over next_process_at scores.
//
//	GET /api/queues/{qname}/retry_histogram?window=1800&buckets=12
func newRetryHistogramHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qname := mux.Vars(r)["qname"]
		q := r.URL.Query()
		window := clampInt(q.Get("window"), defaultRetryWindowSeconds, 60, 86400)
		buckets := clampInt(q.Get("buckets"), defaultRetryBuckets, 1, 60)
		bucketSeconds := window / buckets
		if bucketSeconds < 1 {
			bucketSeconds = 1
		}

		now := time.Now()
		key := retryZSetKey(qname)
		ctx := r.Context()

		pipe := rc.Pipeline()
		totalCmd := pipe.ZCard(ctx, key)
		// Strictly before now: "(<now>" is an exclusive bound.
		nowStr := strconv.FormatInt(now.Unix(), 10)
		pastDueCmd := pipe.ZCount(ctx, key, "-inf", "("+nowStr)
		bucketCmds := make([]*redis.IntCmd, buckets)
		for i := 0; i < buckets; i++ {
			lo := now.Unix() + int64(i*bucketSeconds)
			hi := now.Unix() + int64((i+1)*bucketSeconds)
			bucketCmds[i] = pipe.ZCount(ctx, key,
				strconv.FormatInt(lo, 10), "("+strconv.FormatInt(hi, 10))
		}
		end := now.Unix() + int64(buckets*bucketSeconds)
		beyondCmd := pipe.ZCount(ctx, key, strconv.FormatInt(end, 10), "+inf")
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		counts := make([]int64, buckets)
		for i, cmd := range bucketCmds {
			counts[i], _ = cmd.Result()
		}
		total, _ := totalCmd.Result()
		pastDue, _ := pastDueCmd.Result()
		beyond, _ := beyondCmd.Result()

		writeResponseJSON(w, retryHistogramResponse{
			Queue:         qname,
			From:          now.Format(time.RFC3339),
			BucketSeconds: bucketSeconds,
			PastDue:       pastDue,
			Buckets:       counts,
			Beyond:        beyond,
			Total:         total,
			Exact:         true,
		})
	}
}

// ----------------------------------------------------------------------------
// GET /api/queues/{qname}/pending_wait_sample
// ----------------------------------------------------------------------------

const (
	defaultWaitProbesPerBand = 20
	maxWaitProbesPerBand     = 50
)

// pendingWaitMethodLabel is the §3.3(c) honesty caption, rendered verbatim
// by the UI. Do not reword without frontend sign-off.
const pendingWaitMethodLabel = "sampled, head/tail-biased"

type pendingWaitSampleResponse struct {
	Queue   string `json:"queue"`
	ListLen int64  `json:"list_len"`
	// Probes actually issued per band (bands overlap-deduped on short lists).
	HeadProbes int `json:"head_probes"`
	TailProbes int `json:"tail_probes"`
	// Sampled is the number of waits recovered. It can be lower than the
	// probe count: a task dequeued between LINDEX and HGET has no
	// pending_since anymore — absent data is dropped, not guessed.
	Sampled int     `json:"sampled"`
	WaitsMs []int64 `json:"waits_ms"`
	// Method is the honesty label ("sampled, head/tail-biased"): pending is
	// a Redis LIST, LINDEX is O(index) per probe, so probes come from the
	// head (newest) and tail (oldest) bands — never claimed uniform.
	Method    string `json:"method"`
	SampledAt string `json:"sampled_at"`
}

// newPendingWaitSampleHandlerFunc samples how long pending tasks have
// waited: LINDEX probes from the head/tail bands of the pending LIST
// (asynq LPUSHes on enqueue, so index 0 is newest and -1 is oldest), then
// a pipelined pending_since lookup per probed task.
//
//	GET /api/queues/{qname}/pending_wait_sample?probes=20
func newPendingWaitSampleHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qname := mux.Vars(r)["qname"]
		probes := clampInt(r.URL.Query().Get("probes"), defaultWaitProbesPerBand, 1, maxWaitProbesPerBand)

		now := time.Now()
		ctx := r.Context()
		key := pendingListKey(qname)

		listLen, err := rc.LLen(ctx, key).Result()
		if err != nil && err != redis.Nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		// Pick probe indices: the head band [0, probes) and the tail band
		// (len-probes, len-1], deduped when the bands overlap so a short
		// list is sampled exactly once per element.
		seen := make(map[int64]bool)
		var indices []int64
		headProbes, tailProbes := 0, 0
		for i := int64(0); i < int64(probes) && i < listLen; i++ {
			if !seen[i] {
				seen[i] = true
				indices = append(indices, i)
				headProbes++
			}
		}
		for i := int64(0); i < int64(probes); i++ {
			idx := listLen - 1 - i
			if idx < 0 {
				break
			}
			if !seen[idx] {
				seen[idx] = true
				indices = append(indices, idx)
				tailProbes++
			}
		}

		var ids []string
		if len(indices) > 0 {
			pipe := rc.Pipeline()
			cmds := make([]*redis.StringCmd, len(indices))
			for i, idx := range indices {
				cmds[i] = pipe.LIndex(ctx, key, idx)
			}
			if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			for _, cmd := range cmds {
				if id, err := cmd.Result(); err == nil && id != "" {
					ids = append(ids, id)
				}
			}
		}

		// pendingSinceTimes (task_timing.go) pipelines the HGETs; ids whose
		// pending_since vanished mid-request are simply absent.
		since := pendingSinceTimes(ctx, rc, qname, ids)
		waits := make([]int64, 0, len(since))
		for _, t := range since {
			ms := now.Sub(t).Milliseconds()
			if ms < 0 {
				ms = 0
			}
			waits = append(waits, ms)
		}

		writeResponseJSON(w, pendingWaitSampleResponse{
			Queue:      qname,
			ListLen:    listLen,
			HeadProbes: headProbes,
			TailProbes: tailProbes,
			Sampled:    len(waits),
			WaitsMs:    waits,
			Method:     pendingWaitMethodLabel,
			SampledAt:  now.Format(time.RFC3339),
		})
	}
}
