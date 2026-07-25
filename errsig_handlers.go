package asynqmon

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"

	"github.com/hibiken/asynqmon/errsig"
)

// ****************************************************************************
// This file defines the Errors screen endpoints (build contract §3.6/§5.7,
// phase 9), served from the shared errsig store — any replica answers,
// indexer running or not:
//
//   GET /api/errors/signatures?limit=&sort=last_seen|count
//   GET /api/errors/signatures/{sig}
//
// The JSON field names here are FROZEN frontend contracts (ui/src/
// api-errors.ts) — changes require frontend sign-off. Every response carries
// the §3.6 honesty fields: archived contributions are exact, retry
// contributions are sampled, and counts sourced from archive-trimmed queues
// are lower bounds.
// ****************************************************************************

const (
	defaultSigLimit = 50
	maxSigLimit     = 200

	// listSampleRefs / detailSampleRefs are how many sample task refs ride
	// on a list row vs the detail view (§3.6: "5 sample tasks").
	listSampleRefs   = 5
	detailSampleRefs = 10

	// maxCounterQueues bounds the detail endpoint's live per-queue failed-
	// counter reads (a signature's blast radius is normally a handful of
	// queues; this is a safety bound, not an expected case).
	maxCounterQueues = 100
)

type errSigQueueCell struct {
	Queue        string `json:"queue"`
	Type         string `json:"type"`
	Count        int64  `json:"count"` // archived + retry_sampled
	Archived     int64  `json:"archived"`
	RetrySampled int64  `json:"retry_sampled"`
	Sampled      bool   `json:"sampled"` // any retry-sampled contribution
}

type errSigTaskRef struct {
	Queue string `json:"queue"`
	ID    string `json:"id"`
}

type errSigRow struct {
	Sig      string `json:"sig"`
	Template string `json:"template"`
	// Count = archived (exact) + retry_sampled (last resample).
	Count             int64  `json:"count"`
	CountIsLowerBound bool   `json:"count_is_lower_bound"`
	ArchivedCount     int64  `json:"archived_count"`
	RetrySampledCount int64  `json:"retry_sampled_count"`
	FirstSeen         string `json:"first_seen"`
	LastSeen          string `json:"last_seen"`
	// Trend: new | growing | steady | recovering (errsig/trend.go heuristic:
	// first_seen recency + count delta between snapshot windows).
	Trend          string            `json:"trend"`
	Queues         []errSigQueueCell `json:"queues"`
	SampleTaskRefs []errSigTaskRef   `json:"sample_task_refs"`
}

// errSigHonesty is the §3.6 data-honesty ribbon, structured.
type errSigHonesty struct {
	ArchivedExact bool     `json:"archived_exact"`
	RetrySampled  bool     `json:"retry_sampled"`
	TrimmedQueues []string `json:"trimmed_queues"`
}

type listErrSigsResponse struct {
	Signatures      []*errSigRow  `json:"signatures"`
	TotalSignatures int           `json:"total_signatures"`
	Honesty         errSigHonesty `json:"honesty"`
	// CounterFailedToday is the fleet-wide failed-counter sum at the last
	// tail sweep — counter-derived magnitude beside index-derived identity
	// (§5.7 cross-check). Counters are never trimmed.
	CounterFailedToday int64 `json:"counter_failed_today"`
	// IndexCountTotal sums every signature's count — what the index itself
	// accounts for.
	IndexCountTotal int64  `json:"index_count_total"`
	TailIndexedAt   string `json:"tail_indexed_at"`
	RetrySampledAt  string `json:"retry_sampled_at"`
}

type errSigCounterRow struct {
	Queue  string `json:"queue"`
	Failed int64  `json:"failed"`
}

type getErrSigResponse struct {
	Signature *errSigRow    `json:"signature"`
	Honesty   errSigHonesty `json:"honesty"`
	// FailedTodayByQueue are live daily failed counters for the signature's
	// source queues (read at request time — exact, never trimmed). They count
	// ALL failures in those queues, not only this signature's — labeled so
	// in the UI. The count-over-time chart arrives with ring buffers
	// (phase 10); until then these numbers are the honest substitute.
	FailedTodayByQueue     []errSigCounterRow `json:"failed_today_by_queue"`
	FailedTodayQueuesTotal int64              `json:"failed_today_queues_total"`
	CounterFailedToday     int64              `json:"counter_failed_today"`
	IndexCountTotal        int64              `json:"index_count_total"`
	TailIndexedAt          string             `json:"tail_indexed_at"`
	RetrySampledAt         string             `json:"retry_sampled_at"`
	CountOverTimeAvailable bool               `json:"count_over_time_available"`
}

// trimmedSet folds the meta's trimmed queues into a lookup.
func trimmedSet(meta *errsig.Meta) map[string]bool {
	out := make(map[string]bool, len(meta.TrimmedQueues))
	for _, q := range meta.TrimmedQueues {
		out[q] = true
	}
	return out
}

// buildErrSigRow renders one signature with its refs and honesty flags.
func buildErrSigRow(s *errsig.Signature, refs []errsig.TaskRef, trimmed map[string]bool, now time.Time) *errSigRow {
	row := &errSigRow{
		Sig:               s.Sig,
		Template:          s.Template,
		Count:             s.TotalCount(),
		ArchivedCount:     s.ArchivedCount,
		RetrySampledCount: s.RetrySampledTotal(),
		FirstSeen:         fmtTime(s.FirstSeen),
		LastSeen:          fmtTime(s.LastSeen),
		Trend:             errsig.ComputeTrend(s.FirstSeen, s.LastSeen, s.TotalCount(), s.PrevCount, now),
		Queues:            make([]errSigQueueCell, 0, len(s.Counts)),
		SampleTaskRefs:    make([]errSigTaskRef, 0, len(refs)),
	}
	for _, c := range s.Counts {
		if trimmed[c.Queue] {
			row.CountIsLowerBound = true
		}
		row.Queues = append(row.Queues, errSigQueueCell{
			Queue:        c.Queue,
			Type:         c.Type,
			Count:        c.Archived + c.RetrySampled,
			Archived:     c.Archived,
			RetrySampled: c.RetrySampled,
			Sampled:      c.RetrySampled > 0,
		})
	}
	for _, r := range refs {
		row.SampleTaskRefs = append(row.SampleTaskRefs, errSigTaskRef{Queue: r.Queue, ID: r.ID})
	}
	return row
}

// readRefsFor pipelines the top sample refs for the given signatures.
func readRefsFor(ctx context.Context, rc redis.UniversalClient, sigs []*errsig.Signature, limit int) (map[string][]errsig.TaskRef, error) {
	out := make(map[string][]errsig.TaskRef, len(sigs))
	for _, s := range sigs {
		refs, err := errsig.ReadRefs(ctx, rc, s.Sig, limit)
		if err != nil {
			return nil, err
		}
		out[s.Sig] = refs
	}
	return out, nil
}

func newListErrorSignaturesHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sortKey := q.Get("sort")
		if sortKey == "" {
			sortKey = "last_seen"
		}
		if sortKey != "last_seen" && sortKey != "count" {
			writeErrorMsg(w, http.StatusBadRequest, `sort must be "last_seen" or "count"`)
			return
		}
		limit := atoiDefault(q.Get("limit"), defaultSigLimit)
		if limit < 1 {
			limit = defaultSigLimit
		}
		if limit > maxSigLimit {
			limit = maxSigLimit
		}

		sigs, meta, err := errsig.ReadIndex(r.Context(), rc)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		now := time.Now()
		trimmed := trimmedSet(meta)

		var indexTotal int64
		for _, s := range sigs {
			indexTotal += s.TotalCount()
		}
		switch sortKey {
		case "count":
			sort.Slice(sigs, func(i, j int) bool {
				if sigs[i].TotalCount() != sigs[j].TotalCount() {
					return sigs[i].TotalCount() > sigs[j].TotalCount()
				}
				return sigs[i].Sig < sigs[j].Sig
			})
		default: // last_seen desc
			sort.Slice(sigs, func(i, j int) bool {
				if !sigs[i].LastSeen.Equal(sigs[j].LastSeen) {
					return sigs[i].LastSeen.After(sigs[j].LastSeen)
				}
				return sigs[i].Sig < sigs[j].Sig
			})
		}
		total := len(sigs)
		if len(sigs) > limit {
			sigs = sigs[:limit]
		}

		refsBySig, err := readRefsFor(r.Context(), rc, sigs, listSampleRefs)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		rows := make([]*errSigRow, 0, len(sigs))
		for _, s := range sigs {
			rows = append(rows, buildErrSigRow(s, refsBySig[s.Sig], trimmed, now))
		}

		writeResponseJSON(w, listErrSigsResponse{
			Signatures:      rows,
			TotalSignatures: total,
			Honesty: errSigHonesty{
				ArchivedExact: true,
				RetrySampled:  true,
				TrimmedQueues: append([]string{}, meta.TrimmedQueues...),
			},
			CounterFailedToday: meta.FailedTodayTotal,
			IndexCountTotal:    indexTotal,
			TailIndexedAt:      fmtTime(meta.TailAt),
			RetrySampledAt:     fmtTime(meta.SampleAt),
		})
	}
}

func newGetErrorSignatureHandlerFunc(rc redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sig := mux.Vars(r)["sig"]
		s, err := errsig.ReadSignature(r.Context(), rc, sig)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		if s == nil {
			writeErrorMsg(w, http.StatusNotFound, "no error signature with this hash")
			return
		}
		meta, err := errsig.ReadMeta(r.Context(), rc)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		now := time.Now()
		trimmed := trimmedSet(meta)

		refs, err := errsig.ReadRefs(r.Context(), rc, sig, detailSampleRefs)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		row := buildErrSigRow(s, refs, trimmed, now)

		// Live per-queue failed counters for the blast radius (bounded).
		queues := s.Queues()
		if len(queues) > maxCounterQueues {
			queues = queues[:maxCounterQueues]
		}
		counters := make([]errSigCounterRow, 0, len(queues))
		var queuesTotal int64
		if len(queues) > 0 {
			pipe := rc.Pipeline()
			cmds := make([]*redis.StringCmd, len(queues))
			for i, qn := range queues {
				cmds[i] = pipe.Get(r.Context(), errsig.FailedTodayKey(qn, now))
			}
			if _, perr := pipe.Exec(r.Context()); perr != nil && !errors.Is(perr, redis.Nil) {
				writeError(w, errorStatus(perr), perr)
				return
			}
			for i, qn := range queues {
				var n int64
				if v, gerr := cmds[i].Result(); gerr == nil {
					n, _ = strconv.ParseInt(v, 10, 64)
				}
				counters = append(counters, errSigCounterRow{Queue: qn, Failed: n})
				queuesTotal += n
			}
		}

		writeResponseJSON(w, getErrSigResponse{
			Signature: row,
			Honesty: errSigHonesty{
				ArchivedExact: true,
				RetrySampled:  true,
				TrimmedQueues: append([]string{}, meta.TrimmedQueues...),
			},
			FailedTodayByQueue:     counters,
			FailedTodayQueuesTotal: queuesTotal,
			CounterFailedToday:     meta.FailedTodayTotal,
			IndexCountTotal:        row.Count,
			TailIndexedAt:          fmtTime(meta.TailAt),
			RetrySampledAt:         fmtTime(meta.SampleAt),
			CountOverTimeAvailable: false, // ring buffers are phase 10
		})
	}
}
