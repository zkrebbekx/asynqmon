// Package jobs implements the Fleet Console bulk-job runner (build contract
// §4.3, §5.4, §5.13, phase 5): every whole-scope mutation is a reviewable,
// query-scoped, background-executed job with a preview phase, per-item
// re-verification at act time, per-item failure reporting, and claim-based
// resumability guarded by a fencing token.
package jobs

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/hibiken/asynqmon/aql"
)

// ****************************************************************************
// This file defines:
//   - the Job model persisted in asynqmon:jobs:<id> (§5.4)
//   - Scope: the v1 predicate (queue/state/free-text/meta) with the Matcher
//     seam phase 6 swaps an AQL-compiled matcher into
//   - verb/state vocabulary + the per-state verb capability matrix
// ****************************************************************************

// Verb is the mutation a job applies to every task matching its scope.
type Verb string

const (
	VerbRun     Verb = "run"
	VerbArchive Verb = "archive"
	VerbDelete  Verb = "delete"
	VerbCancel  Verb = "cancel"
)

// Phase is which half of the §4.3 flow the job is in. Every job is created in
// the preview phase; execution is a separate, explicitly-gated request.
type Phase string

const (
	PhasePreview Phase = "preview"
	PhaseExecute Phase = "execute"
)

// State is the job lifecycle state.
type State string

const (
	StatePreviewing   State = "previewing"    // preview enumeration in progress
	StatePreviewReady State = "preview_ready" // enumeration complete, awaiting execute
	StateRunning      State = "running"       // execute requested; acting (or finishing enumeration)
	StatePaused       State = "paused"        // paused between batches; resumable
	StateCanceled     State = "canceled"      // terminal
	StateDone         State = "done"          // terminal
	StateFailed       State = "failed"        // terminal; Job.Error says why
)

// IsTerminal reports whether the state is final.
func (s State) IsTerminal() bool {
	return s == StateCanceled || s == StateDone || s == StateFailed
}

// CostClass labels how expensive execution is for Redis (§4.3 step 3).
type CostClass string

const (
	// CostCheap: acting removes from ZSETs (or enqueues) — O(log N) per task.
	CostCheap CostClass = "cheap"
	// CostListRemoval: delete/archive on a LIST state (pending/active) is an
	// LREM per task — O(list length) PER TASK of single-threaded Redis time.
	// The preview carries the list length so the UI can disclose the
	// estimated cost and the alternatives.
	CostListRemoval CostClass = "list_removal"
)

// Control-flag values (the "ctl" hash field). Set by the HTTP handlers on any
// replica; honored by the claiming runner between batches.
const (
	CtlPause  = "pause"
	CtlCancel = "cancel"
)

// Matcher is the predicate seam: execution re-fetches every task and asks the
// matcher again immediately before acting (§4.3 step 5). v1's implementation
// is Scope below; phase 6 swaps in an AQL-compiled matcher without touching
// the runner.
type Matcher interface {
	Matches(ti *asynq.TaskInfo) bool
}

// Scope is the job predicate — the existing search endpoint's vocabulary
// (queue, state, free-text q, meta key:value filters) plus, since phase 6,
// an AQL query compiled through the Matcher seam. Legacy fields keep
// working; when Aql is set it is ANDed with them.
type Scope struct {
	// Queue is a queue name, or ""/"all" for every queue.
	Queue string `json:"queue"`
	// State is the task state the scope enumerates. Required.
	State string `json:"state"`
	// Q is a case-insensitive substring match over id, type, queue and the
	// raw payload (same semantics as GET /api/tasks).
	Q string `json:"q"`
	// Meta are "key:value" payload constraints (AND, top-level JSON scalars),
	// in the same wire format the search endpoint accepts.
	Meta []string `json:"meta"`
	// Aql is the console's AQL query (phase 6, §3.4). Compiled via
	// aql.CompileMatcher at enumeration/execution start; env-dependent
	// predicates (running>, deadline_pct>, orphaned, pending_age>,
	// group_age>) are rejected at job-create time because the runner
	// re-verifies from TaskInfo alone. Relative durations anchor to the
	// moment the matcher is compiled.
	Aql string `json:"aql,omitempty"`
}

// scopeStates mirrors the search endpoint's searchable states.
var scopeStates = map[string]bool{
	"active":      true,
	"pending":     true,
	"scheduled":   true,
	"retry":       true,
	"archived":    true,
	"completed":   true,
	"aggregating": true,
}

// verbCaps is the per-state verb capability matrix (same rules the console's
// bulk bar exposes; mirrors what the Inspector verbs actually support).
var verbCaps = map[string]map[Verb]bool{
	"active":      {VerbCancel: true},
	"pending":     {VerbArchive: true, VerbDelete: true},
	"aggregating": {VerbRun: true, VerbArchive: true, VerbDelete: true},
	"scheduled":   {VerbRun: true, VerbArchive: true, VerbDelete: true},
	"retry":       {VerbRun: true, VerbArchive: true, VerbDelete: true},
	"archived":    {VerbRun: true, VerbDelete: true},
	"completed":   {VerbDelete: true},
}

// ValidateScopeVerb checks the scope's state and the verb against the
// capability matrix. Returns a human-readable reason when invalid.
func ValidateScopeVerb(s Scope, v Verb) string {
	if !scopeStates[s.State] {
		return "invalid scope state " + strconv.Quote(s.State)
	}
	caps, ok := verbCaps[s.State]
	if !ok || !caps[v] {
		return "verb " + string(v) + " is not supported for state " + s.State
	}
	return ""
}

// ValidateAql checks the scope's AQL clause: it must parse, be answerable
// in the scope's state, and compile to an env-free matcher (the runner's
// re-verification seam). Returns the positioned rejection for the HTTP 400
// body, or nil.
func (s Scope) ValidateAql() *aql.ParseError {
	if s.Aql == "" {
		return nil
	}
	q, perr := aql.Parse(s.Aql)
	if perr != nil {
		return perr
	}
	if st, ok := q.ExplicitState(); ok && st != s.State {
		return &aql.ParseError{
			Msg:  "scope state " + strconv.Quote(s.State) + " conflicts with the query's state=" + st,
			Pos:  0,
			Hint: "the scope's state and the query's state= clause must agree",
		}
	}
	if _, perr := aql.CompileMatcher(q, s.State, time.Now()); perr != nil {
		return perr
	}
	return nil
}

// Compile builds the scope's full predicate once: the legacy queue/state/
// free-text/meta checks plus the compiled AQL matcher. The runner compiles
// at enumeration/execution start so per-task re-verification never re-parses.
func (s Scope) Compile() (Matcher, *aql.ParseError) {
	var aqlMatch func(*asynq.TaskInfo) bool
	if s.Aql != "" {
		q, perr := aql.Parse(s.Aql)
		if perr != nil {
			return nil, perr
		}
		m, perr := aql.CompileMatcher(q, s.State, time.Now())
		if perr != nil {
			return nil, perr
		}
		aqlMatch = m
	}
	return compiledScope{scope: s, aqlMatch: aqlMatch}, nil
}

// compiledScope is Scope's Matcher: legacy predicate AND compiled AQL.
type compiledScope struct {
	scope    Scope
	aqlMatch func(*asynq.TaskInfo) bool
}

func (m compiledScope) Matches(ti *asynq.TaskInfo) bool {
	if !m.scope.matchesLegacy(ti) {
		return false
	}
	return m.aqlMatch == nil || m.aqlMatch(ti)
}

// ComputeCostClass implements §5.4's rule: delete/archive on a LIST state
// (pending/active) is a selective LREM — cost class list_removal; everything
// else is cheap.
func ComputeCostClass(s Scope, v Verb) CostClass {
	if (v == VerbDelete || v == VerbArchive) && (s.State == "pending" || s.State == "active") {
		return CostListRemoval
	}
	return CostCheap
}

// Matches reports whether the task matches the whole scope, including any
// AQL clause (an unparsable AQL clause matches nothing — the create handler
// validates it, so this only guards corrupted stored scopes). The runner
// uses Compile once instead; this convenience form re-compiles per call.
func (s Scope) Matches(ti *asynq.TaskInfo) bool {
	m, perr := s.Compile()
	if perr != nil {
		return false
	}
	return m.Matches(ti)
}

// matchesLegacy is the pre-AQL predicate. Queue/state are enumeration
// inputs, but they are re-checked here too: execution re-fetches each task,
// and a task that changed state (or was re-enqueued elsewhere) between
// preview and act must count as skipped, never be acted on (§4.3 step 5 —
// the TOCTOU fix).
func (s Scope) matchesLegacy(ti *asynq.TaskInfo) bool {
	if ti == nil {
		return false
	}
	if s.Queue != "" && s.Queue != "all" && ti.Queue != s.Queue {
		return false
	}
	if ti.State.String() != s.State {
		return false
	}
	if !matchesFreeText(ti, s.Q) {
		return false
	}
	return matchesMeta(ti.Payload, s.Meta)
}

// matchesFreeText mirrors the search endpoint: case-insensitive substring
// over id, type, queue and the raw payload bytes.
func matchesFreeText(ti *asynq.TaskInfo, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(ti.ID), q) ||
		strings.Contains(strings.ToLower(ti.Type), q) ||
		strings.Contains(strings.ToLower(ti.Queue), q) ||
		strings.Contains(strings.ToLower(string(ti.Payload)), q)
}

// matchesMeta mirrors the search endpoint's metadata semantics: every
// "key:value" filter must equal a top-level scalar of the JSON payload.
// Non-JSON payloads match only an empty filter set.
func matchesMeta(payload []byte, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	for _, f := range filters {
		i := strings.Index(f, ":")
		if i <= 0 {
			continue // malformed filter — same drop-don't-crash rule as the search parser
		}
		key, want := f[:i], f[i+1:]
		v, ok := obj[key]
		if !ok || scalarString(v) != want {
			return false
		}
	}
	return true
}

// scalarString renders a JSON scalar the way the console chips do; nested
// values render "" and therefore never match.
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
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return ""
	}
}

// Counts is the job's progress accounting (§4.3 step 5: acted / skipped —
// no-longer-matching / failed, plus the streaming candidate count).
type Counts struct {
	Candidates int64 `json:"candidates"`
	Acted      int64 `json:"acted"`
	Skipped    int64 `json:"skipped"`
	Failed     int64 `json:"failed"`
}

// Job is one bulk operation. Persisted as the asynqmon:jobs:<id> hash;
// mutated only through Store (progress writes are fence-guarded, §5.13).
type Job struct {
	ID    string `json:"id"`
	Verb  Verb   `json:"verb"`
	Scope Scope  `json:"scope"`
	Phase Phase  `json:"phase"`
	State State  `json:"state"`

	Throttle int    `json:"throttle"` // tasks/sec during execute
	Reason   string `json:"reason"`   // mandatory (§5.11)
	Actor    string `json:"actor"`

	Counts    Counts    `json:"counts"`
	CostClass CostClass `json:"cost_class"`
	// CostListLen is the LIST length backing the list_removal estimate
	// (fetched once at preview start; 0 for cheap scopes).
	CostListLen int64 `json:"cost_list_len"`

	PreviewComplete  bool `json:"preview_complete"`
	ProceedOnPartial bool `json:"proceed_on_partial"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`  // execute start
	FinishedAt time.Time `json:"finished_at"` // terminal transition

	// Fence is the current fencing token; a claimer whose token is older
	// than this can no longer write (§5.13).
	Fence int64 `json:"fence"`

	Error string `json:"error"` // set when State == failed

	// FailuresOverflow counts per-item failures beyond the 10k report cap.
	FailuresOverflow int64 `json:"failures_overflow"`

	// Ctl is the pending control request (pause/cancel), honored between
	// batches by the claiming runner.
	Ctl string `json:"-"`

	// Enumeration cursor, persisted every batch so a restarted replica
	// resumes instead of rescanning (§5.4). queues is the resolved,
	// order-frozen queue list; groups the current queue's group list
	// (aggregating state only).
	cursorQueues []string
	cursorGroups []string
	cursorQIdx   int
	cursorGIdx   int
	cursorPage   int

	// actCursor is the offset into the candidates list already acted on.
	actCursor int64
}

// SampleRow is one row of the 10-row preview sample.
type SampleRow struct {
	ID      string `json:"id"`
	Queue   string `json:"queue"`
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// ItemFailure is one per-item execution failure (capped report, §3.9).
type ItemFailure struct {
	Queue string `json:"queue"`
	ID    string `json:"id"`
	Error string `json:"error"`
}
