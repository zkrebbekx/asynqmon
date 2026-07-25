package asynqmon

import (
	"sort"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - the coverage join (build contract §5.5): Inspector.Servers() priority
//     maps ⨯ Inspector.Queues() → per-queue consumer rows with UNCOVERED /
//     STARVED flags for the Workers screen (§3.7).
//
// Pure functions over already-fetched data — O(servers + queues-in-configs),
// cheap at any fleet size. The HTTP handler lives in coverage_handlers.go.
// ****************************************************************************

// Coverage row statuses. Severity is color + form in the UI (§3.7): the
// status string is the form cue, never color alone.
const (
	coverageStatusOK        = "ok"
	coverageStatusUncovered = "uncovered"
	coverageStatusStarved   = "starved"
)

// coverageDepth is the slice of a queue's stats-cache snapshot the coverage
// join consumes. A nil depth map means the stats cache was unavailable —
// rows then omit pending, and the flags that need depth data stay unflagged
// (honest degradation, never a guess).
type coverageDepth struct {
	Pending int64
	Active  int64
}

// coverageConsumer is one live server consuming a queue.
type coverageConsumer struct {
	ServerID string `json:"server_id"`
	Host     string `json:"host"`
	PID      int    `json:"pid"`
	// Weight is the queue's priority value in this server's queue map.
	Weight int `json:"weight"`
	// StrictPriority reports whether this server runs strict priority mode
	// (higher-weight queues always drain first).
	StrictPriority bool `json:"strict_priority"`
}

// coverageRow is one queue of the coverage matrix.
type coverageRow struct {
	Queue string `json:"queue"`
	// Pending is the queue's pending depth from the stats cache; omitted
	// (nil) when the cache is unavailable — absent data is not zero.
	Pending *int64 `json:"pending,omitempty"`
	// Consumers lists every live server whose priority map includes this
	// queue, sorted by host then PID. Never nil in JSON (empty array).
	Consumers []*coverageConsumer `json:"consumers"`
	// TotalConcurrency sums the Concurrency of the consuming servers: the
	// upper bound of worker slots that could serve this queue (slots are
	// shared across each server's whole queue map, so this is a ceiling,
	// not a guarantee).
	TotalConcurrency int `json:"total_concurrency"`
	// Status is "ok" | "uncovered" | "starved" (see the heuristics below).
	Status string `json:"status"`
}

// buildCoverageRows joins live servers' priority maps against the known
// queue set. Row set = union of Inspector.Queues() and every queue named in
// a live server's config (a consumer-only queue may not be registered in
// asynq:queues yet — "who consumes Z" must still answer).
//
// UNCOVERED (crit): pending > 0 ∧ zero live consumers (§3.7). Requires known
// depth data — with no stats cache we cannot honestly claim work "will not
// run", so consumer-less queues stay "ok" and the UI still shows the empty
// consumer list.
//
// STARVED (warn) — the shipped heuristic, an honest approximation of §3.7's
// "present only at low weight under StrictPriority with permanently-busy
// higher priorities". A queue is flagged starved iff ALL of:
//
//  1. it has at least one live consumer, and depth data is available;
//  2. EVERY consuming server runs StrictPriority — one fair-weighted
//     consumer means the queue does get turns, so it is not starved;
//  3. on every consuming server the queue sits at that server's minimum
//     weight AND a strictly higher weight class exists there (a queue at
//     the top — or alone, or tied across the whole map — cannot be starved
//     by peers);
//  4. on every consuming server at least one strictly-higher-weight queue
//     is currently non-empty (pending > 0 or active > 0 in the stats
//     cache) — if any server's higher classes are all drained, that server
//     will reach this queue, so it is not starved.
//
// What this deliberately does NOT capture: asynq's strict scheduler starves
// a queue whenever any higher class has work at dequeue time, so a mid-tier
// queue can also starve, and "non-empty right now" is a snapshot, not
// "permanently busy" (no ring-buffer history until phase 10). The narrow
// lowest-weight + currently-non-empty form trades recall for zero false
// "STARVED" chips on queues that are actually being served.
func buildCoverageRows(servers []*asynq.ServerInfo, queues []string, depths map[string]coverageDepth) []*coverageRow {
	// Union of registered queues and every queue in a live server's config.
	names := make(map[string]bool, len(queues))
	for _, q := range queues {
		names[q] = true
	}
	consumersByQueue := make(map[string][]*asynq.ServerInfo)
	for _, s := range servers {
		for q := range s.Queues {
			names[q] = true
			consumersByQueue[q] = append(consumersByQueue[q], s)
		}
	}

	rows := make([]*coverageRow, 0, len(names))
	for q := range names {
		srvs := consumersByQueue[q]
		row := &coverageRow{
			Queue:     q,
			Consumers: make([]*coverageConsumer, 0, len(srvs)),
			Status:    coverageStatusOK,
		}
		for _, s := range srvs {
			row.TotalConcurrency += s.Concurrency
			row.Consumers = append(row.Consumers, &coverageConsumer{
				ServerID:       s.ID,
				Host:           s.Host,
				PID:            s.PID,
				Weight:         s.Queues[q],
				StrictPriority: s.StrictPriority,
			})
		}
		sort.Slice(row.Consumers, func(i, j int) bool {
			if row.Consumers[i].Host != row.Consumers[j].Host {
				return row.Consumers[i].Host < row.Consumers[j].Host
			}
			return row.Consumers[i].PID < row.Consumers[j].PID
		})
		if d, ok := depths[q]; ok {
			p := d.Pending
			row.Pending = &p
		}
		switch {
		case len(srvs) == 0 && row.Pending != nil && *row.Pending > 0:
			row.Status = coverageStatusUncovered
		case isStarved(q, srvs, depths):
			row.Status = coverageStatusStarved
		}
		rows = append(rows, row)
	}

	// Problems first (uncovered, then starved), alphabetical within each
	// band — deterministic across polls.
	rank := map[string]int{coverageStatusUncovered: 0, coverageStatusStarved: 1, coverageStatusOK: 2}
	sort.Slice(rows, func(i, j int) bool {
		if rank[rows[i].Status] != rank[rows[j].Status] {
			return rank[rows[i].Status] < rank[rows[j].Status]
		}
		return rows[i].Queue < rows[j].Queue
	})
	return rows
}

// isStarved implements conditions 1–4 of the starvation heuristic documented
// on buildCoverageRows.
func isStarved(queue string, servers []*asynq.ServerInfo, depths map[string]coverageDepth) bool {
	if len(servers) == 0 || depths == nil {
		return false // no consumers is "uncovered" territory; no depth data means no honest claim
	}
	for _, s := range servers {
		if !s.StrictPriority {
			return false // a fair-weighted consumer gives this queue turns
		}
		w := s.Queues[queue]
		minW, maxW := w, w
		for _, wt := range s.Queues {
			if wt < minW {
				minW = wt
			}
			if wt > maxW {
				maxW = wt
			}
		}
		if w != minW || maxW <= w {
			return false // not in the lowest class, or no higher class exists
		}
		higherBusy := false
		for name, wt := range s.Queues {
			if wt <= w {
				continue
			}
			if d, ok := depths[name]; ok && (d.Pending > 0 || d.Active > 0) {
				higherBusy = true
				break
			}
		}
		if !higherBusy {
			return false // this server's higher classes are drained — it will reach the queue
		}
	}
	return true
}
