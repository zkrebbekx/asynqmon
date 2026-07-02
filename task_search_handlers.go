package asynqmon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - a server-side task search/filter/pagination endpoint that scales beyond
//     what client-side filtering can handle (100k+ tasks).
//
//   GET /api/tasks?queue=&state=&q=&meta=key:val&meta=...&page=&size=&max_scan=
// ****************************************************************************

const (
	defaultMaxScan   = 10000  // cap on tasks scanned per queue per request
	maxScanCeiling   = 100000 // hard upper bound on the per-queue scan cap
	searchBatchSize  = 1000   // page size used while scanning the inspector
	defaultSearchTop = 20     // default result page size
)

// clampMaxScan bounds the user-provided max_scan so a single request cannot
// force an unbounded scan of every task in every queue.
func clampMaxScan(n int) int {
	if n < 1 {
		return defaultMaxScan
	}
	if n > maxScanCeiling {
		return maxScanCeiling
	}
	return n
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
	Total     int           `json:"total"`     // number of matches found (within scan cap)
	Scanned   int           `json:"scanned"`   // number of tasks examined
	Truncated bool          `json:"truncated"` // true if the scan cap was hit (more may exist)
	Page      int           `json:"page"`
	Size      int           `json:"size"`
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
		rawPayload:    DefaultPayloadFormatter.FormatPayload(ti.Type, ti.Payload),
	}
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
		if !ok || scalarString(v) != f.Value {
			return false
		}
	}
	return true
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
	return inspector.Queues()
}

// scanMatchingTasks scans the given queues/state in batches, applies the search
// and metadata filters, and returns all matches found within the per-queue
// max_scan cap (along with how many tasks were examined and whether the cap was
// hit). Shared by the search and facet endpoints.
//
// Queues removed mid-scan are skipped; any other error (e.g. a Redis outage)
// aborts the request so the caller can surface it instead of silently
// returning an empty result set.
func scanMatchingTasks(
	inspector *asynq.Inspector,
	queues []string,
	state, search string,
	metaFilters []metaFilter,
	maxScan int,
	pf PayloadFormatter,
) (matches []*searchTask, scanned int, truncated bool, err error) {
	matches = make([]*searchTask, 0)
	for _, qname := range queues {
		// The aggregating state is per-group; scan every group in the queue.
		groups := []string{""}
		if state == "aggregating" {
			ginfos, gerr := inspector.Groups(qname)
			if gerr != nil {
				if errors.Is(gerr, asynq.ErrQueueNotFound) {
					continue
				}
				return nil, scanned, truncated, gerr
			}
			groups = groups[:0]
			for _, g := range ginfos {
				groups = append(groups, g.Group)
			}
		}
		qScanned := 0
	queueScan:
		for _, gname := range groups {
			pageNum := 1
			for qScanned < maxScan {
				batch, lerr := listTasksByState(inspector, qname, gname, state, pageNum, searchBatchSize)
				if lerr != nil {
					if errors.Is(lerr, asynq.ErrQueueNotFound) {
						break queueScan // queue removed mid-scan; skip it
					}
					return nil, scanned, truncated, lerr
				}
				if len(batch) == 0 {
					break
				}
				for _, ti := range batch {
					st := toSearchTask(ti, pf)
					if taskMatchesSearch(st, search) && taskMatchesMeta(st.rawPayload, metaFilters) {
						matches = append(matches, st)
					}
				}
				scanned += len(batch)
				qScanned += len(batch)
				if len(batch) < searchBatchSize {
					break // reached the end of this queue/group/state
				}
				pageNum++
			}
		}
		if qScanned >= maxScan {
			truncated = true
		}
	}
	return matches, scanned, truncated, nil
}

func newSearchTasksHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		state := q.Get("state")
		if state == "" {
			state = "pending"
		}
		if !searchableStates[state] {
			writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
			return
		}
		search := q.Get("q")
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
		maxScan := clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))

		queues, err := resolveQueues(inspector, q.Get("queue"))
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		matches, scanned, truncated, err := scanMatchingTasks(inspector, queues, state, search, metaFilters, maxScan, pf)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		total := len(matches)
		start := (page - 1) * size
		if start > total {
			start = total
		}
		end := start + size
		if end > total {
			end = total
		}
		pageTasks := matches[start:end]
		if pageTasks == nil {
			pageTasks = make([]*searchTask, 0)
		}

		writeResponseJSON(w, searchTasksResponse{
			Tasks:     pageTasks,
			Total:     total,
			Scanned:   scanned,
			Truncated: truncated,
			Page:      page,
			Size:      size,
		})
	}
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

// collectFacets aggregates distinct top-level scalar key=value pairs across the
// matched tasks, most frequent first, capped at limit.
func collectFacets(matches []*searchTask, limit int) []metaFacet {
	type agg struct {
		facet metaFacet
		n     int
	}
	counts := make(map[string]*agg)
	for _, t := range matches {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(t.rawPayload), &obj); err != nil {
			continue
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
			if a, ok := counts[id]; ok {
				a.n++
			} else {
				counts[id] = &agg{facet: metaFacet{Key: k, Value: val}, n: 1}
			}
		}
	}
	out := make([]metaFacet, 0, len(counts))
	for _, a := range counts {
		f := a.facet
		f.Count = a.n
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

// aggregateBy groups the matched tasks by a chosen field (type/error/queue) and
// returns counts, most frequent first, capped at limit.
func aggregateBy(matches []*searchTask, by string, limit int) []aggregateGroup {
	counts := make(map[string]int)
	for _, t := range matches {
		var label string
		switch by {
		case "type":
			label = t.Type
		case "error":
			label = t.LastError
		case "queue":
			label = t.Queue
		default:
			label = t.Type
		}
		if label == "" {
			continue
		}
		counts[label]++
	}
	out := make([]aggregateGroup, 0, len(counts))
	for label, n := range counts {
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

// newTaskAggregateHandlerFunc groups the filtered task set by type, error, or
// queue — powering failure analytics ("top failing types", "top errors").
//
//	GET /api/task_aggregate?queue=&state=&q=&meta=&by=type|error|queue&max_scan=&limit=
func newTaskAggregateHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		state := q.Get("state")
		if state == "" {
			state = "retry"
		}
		if !searchableStates[state] {
			writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
			return
		}
		by := q.Get("by")
		if by == "" {
			by = "type"
		}
		search := q.Get("q")
		metaFilters := parseMetaFilters(q["meta"])
		maxScan := clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))
		limit := atoiDefault(q.Get("limit"), defaultFacetLimit)
		if limit < 1 {
			limit = defaultFacetLimit
		}

		queues, err := resolveQueues(inspector, q.Get("queue"))
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		matches, scanned, truncated, err := scanMatchingTasks(inspector, queues, state, search, metaFilters, maxScan, pf)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}
		writeResponseJSON(w, taskAggregateResponse{
			By:        by,
			Groups:    aggregateBy(matches, by, limit),
			Total:     len(matches),
			Scanned:   scanned,
			Truncated: truncated,
		})
	}
}

// newTaskMetadataHandlerFunc returns metadata facets (distinct key=value pairs
// with counts) across the whole filtered result set, so the UI can offer global
// drill-down chips rather than ones limited to the current page.
//
//	GET /api/task_metadata?queue=&state=&q=&meta=key:val&max_scan=&limit=
func newTaskMetadataHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		state := q.Get("state")
		if state == "" {
			state = "pending"
		}
		if !searchableStates[state] {
			writeErrorMsg(w, http.StatusBadRequest, fmt.Sprintf("invalid state %q", state))
			return
		}
		search := q.Get("q")
		metaFilters := parseMetaFilters(q["meta"])
		maxScan := clampMaxScan(atoiDefault(q.Get("max_scan"), defaultMaxScan))
		limit := atoiDefault(q.Get("limit"), defaultFacetLimit)
		if limit < 1 {
			limit = defaultFacetLimit
		}

		queues, err := resolveQueues(inspector, q.Get("queue"))
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		matches, scanned, truncated, err := scanMatchingTasks(inspector, queues, state, search, metaFilters, maxScan, pf)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		writeResponseJSON(w, taskMetadataResponse{
			Facets:    collectFacets(matches, limit),
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
}

type bulkFilteredResponse struct {
	Processed int  `json:"processed"`
	Errors    int  `json:"errors"`
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated"`
}

// newBulkFilteredTasksHandlerFunc applies an action to every task matching a
// queue/state/search/metadata filter (within the scan cap), not just the rows
// on the current page.
//
//	POST /api/tasks:batch_filtered  {queue,state,q,meta,action,max_scan}
func newBulkFilteredTasksHandlerFunc(inspector *asynq.Inspector, pf PayloadFormatter) http.HandlerFunc {
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
		maxScan := clampMaxScan(req.MaxScan)

		queues, err := resolveQueues(inspector, req.Queue)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		matches, scanned, truncated, err := scanMatchingTasks(inspector, queues, req.State, req.Q, parseMetaFilters(req.Meta), maxScan, pf)
		if err != nil {
			writeError(w, errorStatus(err), err)
			return
		}

		processed, errCount := 0, 0
		for _, t := range matches {
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
