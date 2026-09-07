package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"

	"github.com/zkrebbekx/asynqmon/aql"
	"github.com/zkrebbekx/asynqmon/jobs"
	"github.com/zkrebbekx/asynqmon/stats"
)

// ****************************************************************************
// This file defines:
//   - a server-side task search/filter/pagination endpoint that scales beyond
//     what client-side filtering can handle (100k+ tasks).
//
//   GET /api/tasks?queue=&state=&q=&meta=key:val&meta=...&page=&size=&max_scan=
//
// Phase 6 (§3.4, §5.3, §5.9): `q` may now be an AQL query. Clause-shaped
// strings are parsed, per-state gated, and compiled to either an exact
// cursor listing (score-cursorable predicates) or a budgeted progressive
// scan with an explicit resume cursor. Plain free-text `q` values keep the
// legacy substring semantics untouched. Also:
//
//   GET /api/tasks/state_counts?queue=   — all seven per-state counts in one
//   pipelined pass (fleet-wide = stats cache sums)
// ****************************************************************************

const (
	defaultMaxScan            = 10000 // cap on tasks scanned per request, TOTAL across queues
	defaultMaxScanCeiling     = 20000 // default Options.MaxScanCeiling (review #27)
	defaultMaxConcurrentScans = 4     // default Options.MaxConcurrentScans (review #27)
	searchBatchSize           = 1000  // page size used while scanning the inspector
	defaultSearchTop          = 20    // default result page size

	// batchFilteredMaxMatches caps the synchronous batch_filtered path. A
	// filter that matches more tasks is refused with 400 and pointed at
	// POST /api/jobs, which previews, audits and throttles large sets as a
	// leased background job (review #31).
	batchFilteredMaxMatches = 2000
)

// scanGate is the process-wide limit on concurrent task scans plus the
// ceiling on a request's max_scan (Options.MaxConcurrentScans and
// Options.MaxScanCeiling, review #27). One gate is shared by GET /api/tasks,
// /api/task_metadata, /api/task_aggregate and POST /api/tasks:batch_filtered,
// on both the legacy and the AQL scan-plan paths. A scan that finds the
// gate full is refused with 429 instead of queued: an operator's retry costs
// one round trip, while a queue of blocked handlers would pin memory and
// goroutines for every open Tasks tab.
type scanGate struct {
	sem     chan struct{}
	ceiling int
}

func newScanGate(maxConcurrent, ceiling int) *scanGate {
	if maxConcurrent < 1 {
		maxConcurrent = defaultMaxConcurrentScans
	}
	if ceiling < 1 {
		ceiling = defaultMaxScanCeiling
	}
	return &scanGate{sem: make(chan struct{}, maxConcurrent), ceiling: ceiling}
}

// tryAcquire takes a scan slot without blocking; false when every slot is
// busy. Every true must be paired with one release.
func (g *scanGate) tryAcquire() bool {
	select {
	case g.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *scanGate) release() { <-g.sem }

// clampMaxScan bounds the user-provided max_scan so a single request cannot
// force an unbounded scan of every task in every queue. The default is also
// clamped, so a ceiling below defaultMaxScan still holds.
func (g *scanGate) clampMaxScan(n int) int {
	if n < 1 {
		n = defaultMaxScan
	}
	if n > g.ceiling {
		return g.ceiling
	}
	return n
}

// writeScanBusy is the 429 every scan endpoint answers when the gate is full.
func writeScanBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeErrorMsg(w, http.StatusTooManyRequests, "too many concurrent scans")
}

// batchFilteredLimiter throttles the synchronous batch_filtered action loop
// at the jobs runner's maximum rate, shared by every request in the process
// (review #31): N parallel requests share one 1000/s budget instead of
// getting N x 1000/s. The burst is one tenth of a second of budget, so a
// small selection still finishes in one go.
var batchFilteredLimiter = rate.NewLimiter(rate.Limit(batchFilteredActionsPerSecond), batchFilteredActionsPerSecond/10)

const batchFilteredActionsPerSecond = 1000

// matchSink receives every match a scan produces, in scan order. It returns
// false to stop the scan early (the batch_filtered cap). Scans stream: no
// caller holds every matched row any more (review #27).
type matchSink func(st *searchTask) bool

// pageWindow is the [start, end) index window one result page covers, with
// overflow-safe arithmetic for hostile page values (page-1)*size.
func pageWindow(page, size int) (start, end int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		return 0, 0
	}
	if page-1 > math.MaxInt/size {
		return math.MaxInt, math.MaxInt
	}
	start = (page - 1) * size
	end = start + size
	if end < start {
		end = math.MaxInt
	}
	return start, end
}

// pageCollector keeps only the matches inside one page window and counts
// the rest, so a scan over N matches holds at most `size` formatted rows.
type pageCollector struct {
	start, end int
	total      int
	rows       []*searchTask
}

func newPageCollector(page, size int) *pageCollector {
	start, end := pageWindow(page, size)
	return &pageCollector{start: start, end: end, rows: make([]*searchTask, 0)}
}

func (c *pageCollector) sink(st *searchTask) bool {
	if c.total >= c.start && c.total < c.end {
		c.rows = append(c.rows, st)
	}
	c.total++
	return true
}

// boundedCollector keeps up to limit matches and stops the scan on the
// first match beyond it (overflow), for the batch_filtered cap.
type boundedCollector struct {
	limit    int
	rows     []*searchTask
	overflow bool
}

func (c *boundedCollector) sink(st *searchTask) bool {
	if len(c.rows) >= c.limit {
		c.overflow = true
		return false
	}
	c.rows = append(c.rows, st)
	return true
}

// pageBounds returns the [start, end) window for one result page, clamped to
// [0, total]. The multiplication (page-1)*size can overflow for hostile page
// values, so it only runs once page-1 is known to fit inside total/size.
func pageBounds(total, page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	start := total
	if size > 0 && page-1 <= total/size {
		start = (page - 1) * size
	}
	end := start + size
	if end > total || end < start {
		end = total
	}
	return start, end
}

// searchableStates are the task states accepted by the search/facet/aggregate/
// bulk-filtered endpoints.
var searchableStates = map[string]bool{
	"active":      true,
	"pending":     true,
	"scheduled":   true,
	"retry":       true,
	"archived":    true,
	"completed":   true,
	"aggregating": true,
}

// searchTask is a unified, state-agnostic task shape returned by the search
// endpoint. JSON tags match the frontend TaskInfo interface.
type searchTask struct {
	ID            string `json:"id"`
	Queue         string `json:"queue"`
	Type          string `json:"type"`
	Payload       string `json:"payload"`
	State         string `json:"state"`
	MaxRetry      int    `json:"max_retry"`
	Retried       int    `json:"retried"`
	LastError     string `json:"error_message"`
	NextProcessAt string `json:"next_process_at"`
	LastFailedAt  string `json:"last_failed_at"`
	CompletedAt   string `json:"completed_at"`

	// rawPayload is the full, untruncated payload used for matching and facets.
	// The Payload field above goes through the (possibly truncating) formatter
	// and is for display only — filtering on it would silently miss tasks whose
	// payload exceeds the truncation limit.
	rawPayload string
}

type searchTasksResponse struct {
	Tasks     []*searchTask `json:"tasks"`
	Total     int64         `json:"total"`     // matches found (exact in cursor mode; within budget otherwise)
	Scanned   int           `json:"scanned"`   // number of tasks examined
	Truncated bool          `json:"truncated"` // back-compat: true when the scan stopped before completion
	Page      int           `json:"page"`
	Size      int           `json:"size"`

	// Phase 6 additions (§3.4, §5.3, §5.9). Zero-valued on the legacy path.
	Mode              string `json:"mode,omitempty"`        // "cursor" | "scan" | "legacy"
	Exact             bool   `json:"exact"`                 // total is exact (cursor mode)
	State             string `json:"state,omitempty"`       // resolved state
	Cursor            string `json:"cursor,omitempty"`      // cursor-mode next page ("" = last page)
	ScanCursor        string `json:"scan_cursor,omitempty"` // scan-mode resume ("" = scan complete)
	CandidateEstimate int64  `json:"candidate_estimate,omitempty"`
	Budget            int    `json:"budget,omitempty"` // scan budget applied this call

	// PendingSinceUnknown counts scanned pending tasks that carry no
	// pending_since record and were therefore not evaluated by pending_age>
	// (RunTask/RunAll/shutdown-requeue gap, review #48). Scan mode only.
	PendingSinceUnknown int `json:"pending_since_unknown,omitempty"`
}

// metaFilter is a single key=value payload constraint.
type metaFilter struct {
	Key   string
	Value string
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func toSearchTask(ti *asynq.TaskInfo, pf PayloadFormatter) *searchTask {
	return &searchTask{
		ID:            ti.ID,
		Queue:         ti.Queue,
		Type:          ti.Type,
		Payload:       pf.FormatPayload(ti.Type, ti.Payload),
		State:         ti.State.String(),
		MaxRetry:      ti.MaxRetry,
		Retried:       ti.Retried,
		LastError:     ti.LastErr,
		NextProcessAt: fmtTime(ti.NextProcessAt),
		LastFailedAt:  fmtTime(ti.LastFailedAt),
		CompletedAt:   fmtTime(ti.CompletedAt),
		rawPayload:    searchablePayload(ti.Type, ti.Payload),
	}
}

// searchablePayload is the text free-text/meta matching runs against: the
// default formatter's output for printable payloads, but the raw bytes for
// binary ones — the formatter's "non-printable bytes" placeholder used to be
// matched instead, so q=non-printable matched every binary task and no real
// byte content was searchable (the AQL path matches raw bytes; the two query
// paths now agree).
func searchablePayload(taskType string, payload []byte) string {
	formatted := DefaultPayloadFormatter.FormatPayload(taskType, payload)
	if formatted == "non-printable bytes" {
		return string(payload)
	}
	return formatted
}

// taskMatchesSearch reports whether the task matches the free-text query
// (case-insensitive substring over id, type, queue, and the full raw payload).
func taskMatchesSearch(t *searchTask, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(t.ID), q) ||
		strings.Contains(strings.ToLower(t.Type), q) ||
		strings.Contains(strings.ToLower(t.Queue), q) ||
		strings.Contains(strings.ToLower(t.rawPayload), q)
}

// taskMatchesMeta reports whether the task's JSON payload contains every
// required key=value pair (AND). Non-JSON payloads only match an empty filter.
// A numeric payload value compares numerically against the filter value
// (10 = 10.0 = 1e1), like the AQL meta.KEY= clause. An integer beyond 2^53
// cannot match exactly: the JSON decode is float64, so neighbouring integers
// share one value (review #54.6).
func taskMatchesMeta(payload string, filters []metaFilter) bool {
	if len(filters) == 0 {
		return true
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return false
	}
	for _, f := range filters {
		v, ok := obj[f.Key]
		if !ok || !metaValueMatches(v, f.Value) {
			return false
		}
	}
	return true
}

// metaValueMatches compares one decoded JSON value against a filter string:
// numbers numerically (when the filter parses as a number), everything else
// through scalarString.
func metaValueMatches(v interface{}, want string) bool {
	if f, ok := v.(float64); ok {
		wantF, err := strconv.ParseFloat(want, 64)
		return err == nil && wantF == f
	}
	return scalarString(v) == want
}

// scalarString renders a JSON scalar the same way the frontend chips do; nested
// values never match.
func scalarString(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		// Render integers without a trailing ".0".
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return ""
	}
}

func parseMetaFilters(values []string) []metaFilter {
	out := make([]metaFilter, 0, len(values))
	for _, v := range values {
		// format: key:value (split on first ':')
		i := strings.Index(v, ":")
		if i <= 0 {
			continue
		}
		out = append(out, metaFilter{Key: v[:i], Value: v[i+1:]})
	}
	return out
}

// listTasksByState returns a page of tasks for the given state.
// gname is only used for the "aggregating" state.
func listTasksByState(inspector *asynq.Inspector, qname, gname, state string, page, size int) ([]*asynq.TaskInfo, error) {
	opts := []asynq.ListOption{asynq.Page(page), asynq.PageSize(size)}
	switch state {
	case "active":
		return inspector.ListActiveTasks(qname, opts...)
	case "pending":
		return inspector.ListPendingTasks(qname, opts...)
	case "scheduled":
		return inspector.ListScheduledTasks(qname, opts...)
	case "retry":
		return inspector.ListRetryTasks(qname, opts...)
	case "archived":
		return inspector.ListArchivedTasks(qname, opts...)
	case "completed":
		return inspector.ListCompletedTasks(qname, opts...)
	case "aggregating":
		return inspector.ListAggregatingTasks(qname, gname, opts...)
	default:
		return nil, fmt.Errorf("unsupported task state %q", state)
	}
}

// resolveQueues maps the queue query param ("" or "all" -> every queue) to a
// concrete list of queue names.
func resolveQueues(inspector *asynq.Inspector, queueParam string) ([]string, error) {
	if queueParam != "" && queueParam != "all" {
		return []string{queueParam}, nil
	}
	qnames, err := inspector.Queues()
	if err != nil {
		return nil, err
	}
	// Queues() order (Redis SMEMBERS) is unspecified. Scanning in a stable
	// order matters here beyond cosmetics: results are concatenated per queue
	// and then paginated, so an unstable order lets a task appear on two pages
	// or on none.
	sort.Strings(qnames)
	return qnames, nil
}

// scanMatchingTasks scans the given queues/state in batches, applies the
// search and metadata filters, and streams every match into sink, within the
// max_scan cap — a TOTAL budget across all queues, like the AQL scan path. It
// returns how many tasks matched, how many were examined, and whether the
// scan stopped before completion. A per-queue budget would multiply by fleet
// size: queue=all on a large fleet could walk max_scan x #queues tasks.
// Shared by the search, facet, aggregate and batch_filtered endpoints.
//
// The scan holds one listing batch at a time; the sink decides what to keep
// (review #27). truncated is true only when the budget stopped the scan
// while work remained: the last batch was full, or later groups/queues were
// never listed (review #54.5). The scan stops at ctx.Err() per page, so a
// client that disconnects stops burning Redis and memory (review #39).
//
// Queues removed mid-scan are skipped; any other error (e.g. a Redis outage)
// aborts the request so the caller can surface it instead of silently
// returning an empty result set.
func scanMatchingTasks(
	ctx context.Context,
	inspector *asynq.Inspector,
	queues []string,
	state, search string,
	metaFilters []metaFilter,
	maxScan int,
	pf PayloadFormatter,
	sink matchSink,
) (matched, scanned int, truncated bool, err error) {
	for qi, qname := range queues {
		// The aggregating state is per-group; scan every group in the queue.
		groups := []string{""}
		if state == "aggregating" {
			ginfos, gerr := inspector.Groups(qname)
			if gerr != nil {
				if errors.Is(gerr, asynq.ErrQueueNotFound) {
					continue
				}
				return matched, scanned, false, gerr
			}
			groups = groups[:0]
			for _, g := range ginfos {
				groups = append(groups, g.Group)
			}
			// Group listing is also SMEMBERS-backed; sort for stable pagination.
			sort.Strings(groups)
		}
	queueScan:
		for gi, gname := range groups {
			pageNum := 1
			for {
				if scanned >= maxScan {
					// Reached only after a full batch: more pages may remain.
					return matched, scanned, true, nil
				}
				if cerr := ctx.Err(); cerr != nil {
					return matched, scanned, true, cerr
				}
				batch, lerr := listTasksByState(inspector, qname, gname, state, pageNum, searchBatchSize)
				if lerr != nil {
					if errors.Is(lerr, asynq.ErrQueueNotFound) {
						break queueScan // queue removed mid-scan; skip it
					}
					return matched, scanned, false, lerr
				}
				if len(batch) == 0 {
					break
				}
				for _, ti := range batch {
					st := toSearchTask(ti, pf)
					if taskMatchesSearch(st, search) && taskMatchesMeta(st.rawPayload, metaFilters) {
						matched++
						if !sink(st) {
							return matched, scanned + len(batch), true, nil
						}
					}
				}
				scanned += len(batch)
				if len(batch) < searchBatchSize {
					break // reached the end of this queue/group/state
				}
				pageNum++
			}
			if scanned >= maxScan && (gi+1 < len(groups) || qi+1 < len(queues)) {
				return matched, scanned, true, nil // budget spent with scope left
			}
		}
	}
	return matched, scanned, false, nil
}

// writeAqlError writes the structured 400 rejection body the console's
// inline banner renders: {error, position, hint} (frontend contract).
func writeAqlError(w http.ResponseWriter, perr *aql.ParseError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(perr)
}

func newSearchTasksHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, statsEngine *stats.Engine, pf PayloadFormatter, scans *scanGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		search := q.Get("q")

		// AQL path: clause-shaped queries get parse-time state gating,
		// cursor listings, and budgeted progressive scans. Plain free text
		// keeps the pre-AQL substring semantics below, so old URLs and old
		// clients behave identically.
		if aql.IsQuery(search) {
			serveAqlSearch(w, r, inspector, rc, statsEngine, pf, scans, search)
			return
		}

		state := q.Get("state")
		if state == "" {
			state = "pending"
		}
		if !searchableStates[state] {
			writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
			return
		}
		metaFilters := parseMetaFilters(q["meta"])

		page := atoiDefault(q.Get("page"), 1)
		if page < 1 {
			page = 1
		}
		size := atoiDefault(q.Get("size"), defaultSearchTop)
		if size < 1 {
			size = defaultSearchTop
		}
		if size > maxPageSize {
			size = maxPageSize
		}
		maxScan := scans.clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))

		if !scans.tryAcquire() {
			writeScanBusy(w)
			return
		}
		defer scans.release()

		queues, err := resolveQueues(inspector, q.Get("queue"))
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		collector := newPageCollector(page, size)
		total, scanned, truncated, err := scanMatchingTasks(r.Context(), inspector, queues, state, search, metaFilters, maxScan, pf, collector.sink)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		writeResponseJSON(w, searchTasksResponse{
			Tasks:     collector.rows,
			Total:     int64(total),
			Scanned:   scanned,
			Truncated: truncated,
			Page:      page,
			Size:      size,
			Mode:      "legacy",
			State:     state,
		})
	}
}

// serveAqlSearch handles GET /api/tasks when q is an AQL query: parse →
// resolve state (explicit clause > legacy state param > inference) →
// compile → execute as an exact cursor listing or a budgeted scan.
func serveAqlSearch(w http.ResponseWriter, r *http.Request, inspector *asynq.Inspector, rc redis.UniversalClient, statsEngine *stats.Engine, pf PayloadFormatter, scans *scanGate, search string) {
	q := r.URL.Query()
	now := time.Now()

	parsed, perr := aql.Parse(search)
	if perr != nil {
		writeAqlError(w, perr)
		return
	}
	state, perr := parsed.ResolveState(q.Get("state"))
	if perr != nil {
		writeAqlError(w, perr)
		return
	}
	plan, perr := aql.Compile(parsed, state, now)
	if perr != nil {
		writeAqlError(w, perr)
		return
	}

	// Queue precedence: an explicit queue= clause wins; the legacy queue
	// param is honored otherwise.
	queueParam := plan.Queue
	if queueParam == "" {
		queueParam = q.Get("queue")
	}
	queues, err := resolveQueues(inspector, queueParam)
	if err != nil {
		writeError(w, errorStatus(err), err)
		return
	}

	size := atoiDefault(q.Get("size"), defaultSearchTop)
	if size < 1 {
		size = defaultSearchTop
	}
	if size > maxPageSize {
		size = maxPageSize
	}

	// Legacy meta params are still honored (ANDed); their presence forces a
	// scan because they need payload decode.
	metaFilters := parseMetaFilters(q["meta"])
	var extra func(*searchTask) bool
	if len(metaFilters) > 0 {
		extra = func(st *searchTask) bool { return taskMatchesMeta(st.rawPayload, metaFilters) }
	}

	if plan.Mode == aql.ModeCursor && extra == nil {
		tasks, total, next, cerr := execCursorPlan(r.Context(), rc, inspector, plan, queues, q.Get("cursor"), size, pf)
		if cerr != nil {
			writeError(w, errorStatus(cerr), cerr)
			return
		}
		writeResponseJSON(w, searchTasksResponse{
			Tasks:   tasks,
			Total:   total,
			Scanned: len(tasks),
			Page:    1,
			Size:    size,
			Mode:    "cursor",
			Exact:   true,
			State:   state,
			Cursor:  next,
		})
		return
	}

	// Scan mode: budgeted, resumable, gated (review #27).
	budget := scans.clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))
	if !scans.tryAcquire() {
		writeScanBusy(w)
		return
	}
	defer scans.release()

	env, eerr := prepareRequestEnv(r.Context(), rc, inspector, plan, queues, now)
	if eerr != nil {
		writeError(w, errorStatus(eerr), eerr)
		return
	}
	estimate, eerr := estimateCandidates(r.Context(), rc, inspector, statsEngine, state, queues, plan.Group)
	if eerr != nil {
		writeError(w, errorStatus(eerr), eerr)
		return
	}

	// First call (no scan_cursor): legacy in-window pagination over the
	// matches, so existing paging UI keeps working — only the requested
	// page's rows are kept. Continuation calls return the new window's
	// matches wholesale, because the console appends them; that window is
	// bounded by the scan ceiling.
	scanCursor := q.Get("scan_cursor")
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 || scanCursor != "" {
		page = 1
	}
	var collector *pageCollector
	if scanCursor == "" {
		collector = newPageCollector(page, size)
	} else {
		collector = newPageCollector(1, math.MaxInt)
	}
	out, serr := execScanPlan(r.Context(), rc, inspector, plan, env, queues, extra, scanCursor, budget, pf, collector.sink)
	if serr != nil {
		writeError(w, errorStatus(serr), serr)
		return
	}

	writeResponseJSON(w, searchTasksResponse{
		Tasks:               collector.rows,
		Total:               int64(out.matched),
		Scanned:             out.scanned,
		Truncated:           out.cursor != "", // back-compat flag; the cursor is the real signal
		Page:                page,
		Size:                size,
		Mode:                "scan",
		State:               state,
		ScanCursor:          out.cursor,
		CandidateEstimate:   estimate,
		Budget:              budget,
		PendingSinceUnknown: env.PendingSinceUnknown,
	})
}

// ----------------------------------------------------------------------------
// GET /api/tasks/state_counts — all seven per-state counts (§3.4 state pills)
// ----------------------------------------------------------------------------

type stateCountsResponse struct {
	Queue string `json:"queue"`
	// Counts has an entry for every state. Aggregating is -1 when the
	// fleet-wide sum is unknowable from the cache (the snapshot stores group
	// COUNTS, not member totals) and the fleet is too large to enumerate —
	// honesty over a guess.
	Counts      map[string]int64 `json:"counts"`
	Source      string           `json:"source"` // "cache" | "live"
	RefreshedAt string           `json:"refreshed_at"`
}

// aggregatingFleetMaxQueues bounds the fleet-wide aggregating enumeration:
// beyond this many group-bearing queues the count reports -1 (unknown)
// instead of an unbounded pass.
const aggregatingFleetMaxQueues = 100

// stateCountsFleetMaxQueues bounds the live fallback (no stats cache): a
// fleet larger than this must run the stats engine for fleet-wide pills.
const stateCountsFleetMaxQueues = 1000

func newStateCountsHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, statsEngine *stats.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		qname := r.URL.Query().Get("queue")
		if qname != "" && qname != "all" {
			counts, err := stateCountsForQueues(r.Context(), rc, inspector, []string{qname})
			if err != nil {
				writeError(w, errorStatus(err), err)
				return
			}
			// LLEN/ZCARD on missing keys answer 0, so without this check a
			// typoed ?queue= silently reported an all-zero state instead of
			// the 404 every other per-queue endpoint returns. asynq 0.25+
			// producers cache "queue already published" per process: a
			// queue an operator deleted keeps receiving tasks without being
			// re-added to asynq:queues, so absence from the set is only a
			// 404 when every per-state count is zero too (review #44).
			if exists, err := rc.SIsMember(r.Context(), "asynq:queues", qname).Result(); err == nil && !exists && stateCountsAllZero(counts) {
				writeErrorMsg(w, http.StatusNotFound, fmt.Sprintf("queue %q does not exist", qname))
				return
			}
			writeResponseJSON(w, stateCountsResponse{
				Queue: qname, Counts: counts, Source: "live",
				RefreshedAt: time.Now().Format(time.RFC3339),
			})
			return
		}

		// Fleet-wide: stats cache sums when available (§3.4 "fleet-wide =
		// cache sums") …
		if statsEngine != nil {
			if res, err := statsEngine.Read(r.Context()); err == nil && res != nil && res.Fleet != nil {
				counts := map[string]int64{
					"pending":   res.Fleet.Pending,
					"active":    res.Fleet.Active,
					"scheduled": res.Fleet.Scheduled,
					"retry":     res.Fleet.Retry,
					"archived":  res.Fleet.Archived,
					"completed": res.Fleet.Completed,
				}
				counts["aggregating"] = fleetAggregating(inspector, res.Queues)
				writeResponseJSON(w, stateCountsResponse{
					Queue: "all", Counts: counts, Source: "cache",
					RefreshedAt: res.Fleet.RefreshedAt.Format(time.RFC3339),
				})
				return
			}
		}

		// … live pipelined fallback otherwise (small fleets / tests).
		queues, err := inspector.Queues()
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		if len(queues) > stateCountsFleetMaxQueues {
			writeErrorMsg(w, http.StatusServiceUnavailable,
				fmt.Sprintf("counts across all queues need the stats engine above %d queues", stateCountsFleetMaxQueues))
			return
		}
		sort.Strings(queues)
		counts, err := stateCountsForQueues(r.Context(), rc, inspector, queues)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		writeResponseJSON(w, stateCountsResponse{
			Queue: "all", Counts: counts, Source: "live",
			RefreshedAt: time.Now().Format(time.RFC3339),
		})
	}
}

// stateCountsAllZero reports whether every per-state count is zero.
func stateCountsAllZero(counts map[string]int64) bool {
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// fleetAggregating sums group member counts across the cached queues that
// report groups, bounded by aggregatingFleetMaxQueues; -1 when unknowable.
func fleetAggregating(inspector *asynq.Inspector, snaps []*stats.QueueSnapshot) int64 {
	var withGroups []string
	for _, s := range snaps {
		if s.Groups > 0 {
			withGroups = append(withGroups, s.Queue)
		}
	}
	if len(withGroups) > aggregatingFleetMaxQueues {
		return -1
	}
	total, err := aggregatingCount(inspector, withGroups, "")
	if err != nil {
		return -1
	}
	return total
}

// collectAqlMatches runs one budgeted scan of an AQL query for the facet /
// aggregate / bulk-filtered endpoints, streaming every match into sink.
// Always a scan — those endpoints decode payloads regardless of the plan's
// mode. truncated reports whether the budget (or the sink) stopped the scan
// early.
func collectAqlMatches(r *http.Request, inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, search, stateParam, queueParam string, metaFilters []metaFilter, budget int, sink matchSink) (matched, scanned int, truncated bool, aqlErr *aql.ParseError, err error) {
	now := time.Now()
	parsed, perr := aql.Parse(search)
	if perr != nil {
		return 0, 0, false, perr, nil
	}
	state, perr := parsed.ResolveState(stateParam)
	if perr != nil {
		return 0, 0, false, perr, nil
	}
	plan, perr := aql.Compile(parsed, state, now)
	if perr != nil {
		return 0, 0, false, perr, nil
	}
	qParam := plan.Queue
	if qParam == "" {
		qParam = queueParam
	}
	queues, err := resolveQueues(inspector, qParam)
	if err != nil {
		return 0, 0, false, nil, err
	}
	env, err := prepareRequestEnv(r.Context(), rc, inspector, plan, queues, now)
	if err != nil {
		return 0, 0, false, nil, err
	}
	var extra func(*searchTask) bool
	if len(metaFilters) > 0 {
		extra = func(st *searchTask) bool { return taskMatchesMeta(st.rawPayload, metaFilters) }
	}
	out, err := execScanPlan(r.Context(), rc, inspector, plan, env, queues, extra, "", budget, pf, sink)
	if err != nil {
		return 0, 0, false, nil, err
	}
	return out.matched, out.scanned, out.cursor != "", nil, nil
}

// collectMatches runs the scan for the facet / aggregate / bulk-filtered
// endpoints: AQL-shaped q values compile and scan like GET /api/tasks,
// anything else takes the legacy substring path. defaultState applies when
// the legacy path gets no state. A *aql.ParseError or an invalid legacy
// state is answered as 400 here; the caller only sees ok=false.
func collectMatches(w http.ResponseWriter, r *http.Request, inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, search, stateParam, defaultState, queueParam string, metaFilters []metaFilter, maxScan int, sink matchSink) (matched, scanned int, truncated bool, ok bool) {
	if aql.IsQuery(search) {
		matched, scanned, truncated, aqlErr, err := collectAqlMatches(r, inspector, rc, pf, search, stateParam, queueParam, metaFilters, maxScan, sink)
		if aqlErr != nil {
			writeAqlError(w, aqlErr)
			return 0, 0, false, false
		}
		if err != nil {
			writeError(w, errorStatus(err), err)
			return 0, 0, false, false
		}
		return matched, scanned, truncated, true
	}
	state := stateParam
	if state == "" {
		state = defaultState
	}
	if !searchableStates[state] {
		writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
		return 0, 0, false, false
	}
	queues, err := resolveQueues(inspector, queueParam)
	if err != nil {
		writeError(w, errorStatus(err), err)
		return 0, 0, false, false
	}
	matched, scanned, truncated, err = scanMatchingTasks(r.Context(), inspector, queues, state, search, metaFilters, maxScan, pf, sink)
	if err != nil {
		writeError(w, errorStatus(err), err)
		return 0, 0, false, false
	}
	return matched, scanned, truncated, true
}

type metaFacet struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Count int    `json:"count"`
}

type taskMetadataResponse struct {
	Facets    []metaFacet `json:"facets"`
	Scanned   int         `json:"scanned"`
	Truncated bool        `json:"truncated"`
}

const defaultFacetLimit = 50

// facetAgg folds matches into distinct top-level scalar key=value counts as
// the scan streams them, so the facet endpoint never holds the match set.
type facetAgg struct {
	counts map[string]*facetCount
}

type facetCount struct {
	facet metaFacet
	n     int
}

func newFacetAgg() *facetAgg { return &facetAgg{counts: make(map[string]*facetCount)} }

// add folds one match; it always continues the scan.
func (a *facetAgg) add(t *searchTask) bool {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(t.rawPayload), &obj); err != nil {
		return true
	}
	for k, v := range obj {
		val := scalarString(v)
		if v == nil {
			continue
		}
		// Skip nested/complex values (scalarString returns "" for them).
		if _, isObj := v.(map[string]interface{}); isObj {
			continue
		}
		if _, isArr := v.([]interface{}); isArr {
			continue
		}
		id := k + "\x00" + val
		if c, ok := a.counts[id]; ok {
			c.n++
		} else {
			a.counts[id] = &facetCount{facet: metaFacet{Key: k, Value: val}, n: 1}
		}
	}
	return true
}

// result ranks the facets most frequent first, capped at limit.
func (a *facetAgg) result(limit int) []metaFacet {
	// High-cardinality guard: a key whose values are (nearly) all distinct —
	// UUIDs, order ids, unique document ids — cannot drill anything down; it
	// only floods the chip row with count-1 chips. Keep a key's singleton
	// values only while the key stays low-cardinality; keys with many
	// distinct values keep just their repeated (actually filterable) values.
	const facetKeyCardinalityCap = 8
	distinctPerKey := make(map[string]int)
	for _, c := range a.counts {
		distinctPerKey[c.facet.Key]++
	}
	out := make([]metaFacet, 0, len(a.counts))
	for _, c := range a.counts {
		if c.n == 1 && distinctPerKey[c.facet.Key] > facetKeyCardinalityCap {
			continue
		}
		f := c.facet
		f.Count = c.n
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// collectFacets aggregates distinct top-level scalar key=value pairs across
// the matched tasks, most frequent first, capped at limit.
func collectFacets(matches []*searchTask, limit int) []metaFacet {
	a := newFacetAgg()
	for _, t := range matches {
		a.add(t)
	}
	return a.result(limit)
}

type aggregateGroup struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type taskAggregateResponse struct {
	By        string           `json:"by"`
	Groups    []aggregateGroup `json:"groups"`
	Total     int              `json:"total"`
	Scanned   int              `json:"scanned"`
	Truncated bool             `json:"truncated"`
}

// aggregateAgg folds matches into per-label counts for one grouping field
// (type/error/queue) as the scan streams them.
type aggregateAgg struct {
	by     string
	counts map[string]int
}

func newAggregateAgg(by string) *aggregateAgg {
	return &aggregateAgg{by: by, counts: make(map[string]int)}
}

// add folds one match; it always continues the scan.
func (a *aggregateAgg) add(t *searchTask) bool {
	var label string
	switch a.by {
	case "type":
		label = t.Type
	case "error":
		label = t.LastError
	case "queue":
		label = t.Queue
	default:
		label = t.Type
	}
	if label != "" {
		a.counts[label]++
	}
	return true
}

// result ranks the groups most frequent first, capped at limit.
func (a *aggregateAgg) result(limit int) []aggregateGroup {
	out := make([]aggregateGroup, 0, len(a.counts))
	for label, n := range a.counts {
		out = append(out, aggregateGroup{Label: label, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// aggregateBy groups the matched tasks by a chosen field (type/error/queue) and
// returns counts, most frequent first, capped at limit.
func aggregateBy(matches []*searchTask, by string, limit int) []aggregateGroup {
	a := newAggregateAgg(by)
	for _, t := range matches {
		a.add(t)
	}
	return a.result(limit)
}

// newTaskAggregateHandlerFunc groups the filtered task set by type, error, or
// queue — powering failure analytics ("top failing types", "top errors").
// AQL-shaped q values are compiled and scanned like GET /api/tasks (phase 6).
//
//	GET /api/task_aggregate?queue=&state=&q=&meta=&by=type|error|queue&max_scan=&limit=
func newTaskAggregateHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, scans *scanGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		by := q.Get("by")
		if by == "" {
			by = "type"
		}
		search := q.Get("q")
		metaFilters := parseMetaFilters(q["meta"])
		maxScan := scans.clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))
		limit := atoiDefault(q.Get("limit"), defaultFacetLimit)
		if limit < 1 {
			limit = defaultFacetLimit
		}

		if !scans.tryAcquire() {
			writeScanBusy(w)
			return
		}
		defer scans.release()

		// Each batch folds into the per-label counts; no row survives the
		// batch (review #27).
		agg := newAggregateAgg(by)
		matched, scanned, truncated, ok := collectMatches(w, r, inspector, rc, pf, search, q.Get("state"), "retry", q.Get("queue"), metaFilters, maxScan, agg.add)
		if !ok {
			return
		}
		writeResponseJSON(w, taskAggregateResponse{
			By:        by,
			Groups:    agg.result(limit),
			Total:     matched,
			Scanned:   scanned,
			Truncated: truncated,
		})
	}
}

// newTaskMetadataHandlerFunc returns metadata facets (distinct key=value pairs
// with counts) across the whole filtered result set, so the UI can offer global
// drill-down chips rather than ones limited to the current page. AQL-shaped q
// values are compiled and scanned like GET /api/tasks (phase 6).
//
//	GET /api/task_metadata?queue=&state=&q=&meta=key:val&max_scan=&limit=
func newTaskMetadataHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, scans *scanGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		search := q.Get("q")
		metaFilters := parseMetaFilters(q["meta"])
		maxScan := scans.clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))
		limit := atoiDefault(q.Get("limit"), defaultFacetLimit)
		if limit < 1 {
			limit = defaultFacetLimit
		}

		if !scans.tryAcquire() {
			writeScanBusy(w)
			return
		}
		defer scans.release()

		// Each batch folds into the facet counts; no row survives the batch
		// (review #27).
		agg := newFacetAgg()
		_, scanned, truncated, ok := collectMatches(w, r, inspector, rc, pf, search, q.Get("state"), "pending", q.Get("queue"), metaFilters, maxScan, agg.add)
		if !ok {
			return
		}
		writeResponseJSON(w, taskMetadataResponse{
			Facets:    agg.result(limit),
			Scanned:   scanned,
			Truncated: truncated,
		})
	}
}

type bulkFilteredRequest struct {
	Queue   string   `json:"queue"`
	State   string   `json:"state"`
	Q       string   `json:"q"`
	Meta    []string `json:"meta"`
	Action  string   `json:"action"` // delete | run | archive | cancel
	MaxScan int      `json:"max_scan"`
	// Reason is an optional operator-provided note recorded in the audit
	// entry, mirroring the jobs API's audited reason.
	Reason string `json:"reason"`
}

type bulkFilteredResponse struct {
	Processed int  `json:"processed"`
	Errors    int  `json:"errors"`
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated"`
}

// newBulkFilteredTasksHandlerFunc applies an action to every task matching a
// queue/state/search/metadata filter (within the scan cap), not just the rows
// on the current page. AQL-shaped q values are compiled and scanned like
// GET /api/tasks (phase 6). §4.3 note: the jobs API is the first-class bulk
// primitive; this endpoint remains for selection-sized scopes.
//
//	POST /api/tasks:batch_filtered  {queue,state,q,meta,action,max_scan,reason}
func newBulkFilteredTasksHandlerFunc(inspector *asynq.Inspector, rc redis.UniversalClient, pf PayloadFormatter, audit *jobs.Store, scans *scanGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req bulkFilteredRequest
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		switch req.Action {
		case "delete", "run", "archive", "cancel":
		default:
			writeErrorMsg(w, http.StatusBadRequest, "invalid action (want delete|run|archive|cancel)")
			return
		}
		if req.State == "" {
			writeErrorMsg(w, http.StatusBadRequest, "state is required")
			return
		}
		if !searchableStates[req.State] {
			writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", req.State))
			return
		}
		maxScan := scans.clampMaxScan(req.MaxScan)

		// The scan holds a gate slot only while it runs; the action loop
		// below is throttled by the shared limiter instead.
		matches, scanned, truncated, ok := func() ([]*searchTask, int, bool, bool) {
			if !scans.tryAcquire() {
				writeScanBusy(w)
				return nil, 0, false, false
			}
			defer scans.release()
			// At most batchFilteredMaxMatches rows are kept; the first match
			// beyond stops the scan and the request is refused (review #31).
			collector := &boundedCollector{limit: batchFilteredMaxMatches}
			_, scanned, truncated, ok := collectMatches(w, r, inspector, rc, pf, req.Q, req.State, req.State, req.Queue, parseMetaFilters(req.Meta), maxScan, collector.sink)
			if !ok {
				return nil, 0, false, false
			}
			if collector.overflow {
				writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf(
					"the filter matches more than %d tasks; use POST /api/jobs, which previews, audits and throttles large sets as a background job",
					batchFilteredMaxMatches))
				return nil, 0, false, false
			}
			return collector.rows, scanned, truncated, true
		}()
		if !ok {
			return
		}

		// Throttled at the jobs runner's maximum rate through the
		// process-wide limiter, so parallel requests share one budget and a
		// filter matching thousands of tasks cannot hammer Redis with an
		// unbounded burst (review #31).
		processed, errCount := 0, 0
		for _, t := range matches {
			if err := batchFilteredLimiter.Wait(r.Context()); err != nil {
				break // client gone; still audit what was applied so far
			}
			var actErr error
			switch req.Action {
			case "delete":
				actErr = inspector.DeleteTask(t.Queue, t.ID)
			case "run":
				actErr = inspector.RunTask(t.Queue, t.ID)
			case "archive":
				actErr = inspector.ArchiveTask(t.Queue, t.ID)
			case "cancel":
				actErr = inspector.CancelProcessing(t.ID)
			}
			if actErr != nil {
				errCount++
			} else {
				processed++
			}
		}

		// Audit like every other mutation path (§5.11). Uses a background
		// context so a client disconnect cannot erase the trace of actions
		// already applied.
		actor := actorFromContext(r.Context())
		auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := audit.AppendAudit(auditCtx, jobs.AuditEntry{
			Event:  jobs.AuditBatchFiltered,
			Actor:  actor.Name,
			Verb:   req.Action,
			Scope:  jobs.Scope{Queue: req.Queue, State: req.State, Q: req.Q, Meta: req.Meta},
			Reason: strings.TrimSpace(req.Reason),
			Acted:  int64(processed),
			Failed: int64(errCount),
		}); err != nil {
			writeErrorMsg(w, http.StatusInternalServerError,
				fmt.Sprintf("%d task(s) were %sd, but the audit write failed: %v", processed, req.Action, err))
			return
		}

		writeResponseJSON(w, bulkFilteredResponse{
			Processed: processed,
			Errors:    errCount,
			Scanned:   scanned,
			Truncated: truncated,
		})
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
