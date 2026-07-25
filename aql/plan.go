package aql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
)

// ****************************************************************************
// This file defines:
//   - Plan: what a parsed query compiles to for a resolved state —
//     (1) a cursorable zset score range (exact totals, (score,id) cursor
//         pagination, §5.3) when every clause is answerable from the state's
//         zset score alone, or
//     (2) a scan predicate (progressive scan with visible budgets, §5.9)
//         when any clause needs message decode or live env data;
//   - Env: the per-request live data env-predicates read (worker tables,
//     lease orphan sets, pending_since, group scores);
//   - CompileMatcher: the jobs.Matcher seam — env-free predicates only,
//     structured error otherwise.
// ****************************************************************************

// PlanMode says how a compiled query executes.
type PlanMode string

const (
	// ModeCursor: the state's primary predicate maps to a zset score range —
	// exact ZRANGEBYSCORE (score,id) cursor listing, exact ZCOUNT totals.
	ModeCursor PlanMode = "cursor"
	// ModeScan: at least one clause needs msg decode or live env data —
	// progressive scan under an explicit budget.
	ModeScan PlanMode = "scan"
)

// Env is the live data env-predicates read, prepared by the executor per
// request (Workers, Orphans) or per scanned batch (PendingSince,
// GroupEnteredAt). Maps are keyed by EnvKey(queue, taskID). A predicate
// whose env entry is absent reports false — honesty over guessing.
type Env struct {
	Now time.Time

	// Workers: active task → its worker's Started/Deadline (from
	// Inspector.Servers()).
	Workers map[string]WorkerObs

	// Orphans: active tasks whose lease score < now − grace (§3.4).
	Orphans map[string]bool

	// PendingSince: pending task → pending_since (task-hash field).
	PendingSince map[string]time.Time

	// GroupEnteredAt: aggregating task → group zset score.
	GroupEnteredAt map[string]time.Time
}

// WorkerObs is the slice of WorkerInfo a predicate needs.
type WorkerObs struct {
	Started  time.Time
	Deadline time.Time
}

// EnvKey builds the Env map key for a task.
func EnvKey(queue, id string) string { return queue + "\x00" + id }

// Plan is a compiled query for one resolved state.
type Plan struct {
	Mode  PlanMode
	State string
	// Queue is the `queue=` clause value ("" = all queues).
	Queue string

	// Score range on the state's zset, unix seconds, inclusive; ±Inf when
	// unbounded. Meaningful in ModeCursor only.
	ScoreMin, ScoreMax float64

	// Match is the full predicate (both modes; in ModeCursor it degenerates
	// to a state/queue guard for re-fetched tasks).
	Match func(env *Env, ti *asynq.TaskInfo) bool

	// Env requirements — the executor prepares exactly what is flagged.
	NeedsWorkers      bool // running> / deadline_pct>
	NeedsOrphans      bool // orphaned
	NeedsPendingSince bool // pending_age>
	NeedsGroupScores  bool // group_age>

	// Group is the `group=` clause value ("" = all groups; aggregating only).
	Group string
}

// zsetStates maps a state to whether its backing structure is a zset whose
// score the matrix declares cursorable, and which fields read that score.
var scoreFieldsByState = map[string][]string{
	"scheduled": {"next_run", "past_due"},
	"retry":     {"next_run", "past_due"},
	"archived":  {"died"},
	"completed": {"expires"},
}

// Compile compiles the query for the resolved state. The caller must have
// resolved state via ResolveState (or otherwise validated it); a query the
// state cannot answer returns the same positioned rejection Parse would.
// now anchors every relative duration in the query — one clock for the whole
// request, so a cursor page and its total agree.
func Compile(q *Query, state string, now time.Time) (*Plan, *ParseError) {
	if !isState(state) {
		return nil, &ParseError{Msg: fmt.Sprintf("unknown state %q", state), Pos: 0,
			Hint: "one of: " + strings.Join(States, ", ")}
	}
	// Re-gate against the concrete state so Compile is safe standalone.
	for i := range q.Clauses {
		c := &q.Clauses[i]
		if c.Field == "" {
			continue
		}
		if c.Field == "state" {
			if c.Value != state {
				return nil, &ParseError{
					Msg: fmt.Sprintf("query names state=%s but is being compiled for state=%s", c.Value, state),
					Pos: c.Pos,
				}
			}
			continue
		}
		if spec := fieldSpecs[c.Field]; !spec.allowedIn(state) {
			return nil, q.mismatchError(c, []string{state})
		}
	}

	p := &Plan{State: state, ScoreMin: math.Inf(-1), ScoreMax: math.Inf(1)}
	scoreFields := map[string]bool{}
	for _, f := range scoreFieldsByState[state] {
		scoreFields[f] = true
	}

	cursorable := scoreFieldsByState[state] != nil
	var preds []func(env *Env, ti *asynq.TaskInfo) bool

	for i := range q.Clauses {
		c := q.Clauses[i] // copy: closures below must not alias the loop var
		switch {
		case c.Field == "state":
			continue
		case c.Field == "queue":
			p.Queue = c.Value
			qv := c.Value
			preds = append(preds, func(_ *Env, ti *asynq.TaskInfo) bool { return ti.Queue == qv })
			continue
		case c.Field == "group":
			p.Group = c.Value
			// Group membership is an enumeration input (the scan lists the
			// named group); no per-task predicate is expressible from
			// TaskInfo, so the plan carries it as scope only.
			cursorable = false
			continue
		}

		spec := fieldSpecs[c.Field]
		if scoreFields[c.Field] {
			// Fold into the zset score range AND keep the equivalent
			// TaskInfo predicate so scan mode (and job re-verification)
			// applies it too.
			lo, hi := scoreRange(&c, state, now)
			p.ScoreMin = math.Max(p.ScoreMin, lo)
			p.ScoreMax = math.Min(p.ScoreMax, hi)
			preds = append(preds, scorePredicate(&c, state, now))
			continue
		}

		// Any other field forces a scan.
		cursorable = false
		if spec.env {
			switch c.Field {
			case "running", "deadline_pct":
				p.NeedsWorkers = true
			case "orphaned":
				p.NeedsOrphans = true
			case "pending_age":
				p.NeedsPendingSince = true
			case "group_age":
				p.NeedsGroupScores = true
			}
		}
		preds = append(preds, clausePredicate(&c, now))
	}

	if cursorable {
		p.Mode = ModeCursor
	} else {
		p.Mode = ModeScan
	}

	state0 := state
	p.Match = func(env *Env, ti *asynq.TaskInfo) bool {
		if ti == nil || ti.State.String() != state0 {
			return false
		}
		for _, pred := range preds {
			if !pred(env, ti) {
				return false
			}
		}
		return true
	}
	return p, nil
}

// scoreRange maps a score-backed clause to [lo, hi] unix seconds on the
// state's zset.
func scoreRange(c *Clause, state string, now time.Time) (float64, float64) {
	switch c.Field {
	case "next_run": // score = next process at
		bound := c.Time
		if c.IsDur {
			bound = now.Add(c.Dur)
		}
		if c.Op == OpLt {
			return math.Inf(-1), float64(bound.Unix())
		}
		return float64(bound.Unix()), math.Inf(1)
	case "past_due": // score <= now
		return math.Inf(-1), float64(now.Unix())
	case "died": // score = died-at; died>D ⇒ died-at <= now−D
		return math.Inf(-1), float64(now.Add(-c.Dur).Unix())
	case "expires": // score = expire-at; expires<V ⇒ expire-at <= bound
		bound := c.Time
		if c.IsDur {
			bound = now.Add(c.Dur)
		}
		return math.Inf(-1), float64(bound.Unix())
	}
	return math.Inf(-1), math.Inf(1)
}

// scorePredicate is the TaskInfo-side equivalent of scoreRange, used by scan
// mode and job re-verification.
func scorePredicate(c *Clause, state string, now time.Time) func(*Env, *asynq.TaskInfo) bool {
	cc := *c
	switch cc.Field {
	case "next_run":
		bound := cc.Time
		if cc.IsDur {
			bound = now.Add(cc.Dur)
		}
		if cc.Op == OpLt {
			return func(_ *Env, ti *asynq.TaskInfo) bool {
				return !ti.NextProcessAt.IsZero() && !ti.NextProcessAt.After(bound)
			}
		}
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return !ti.NextProcessAt.IsZero() && !ti.NextProcessAt.Before(bound)
		}
	case "past_due":
		return func(env *Env, ti *asynq.TaskInfo) bool {
			return !ti.NextProcessAt.IsZero() && !ti.NextProcessAt.After(envNow(env, now))
		}
	case "died":
		// Archived died-at == LastFailedAt (the archive zset scores by it).
		return func(env *Env, ti *asynq.TaskInfo) bool {
			return !ti.LastFailedAt.IsZero() && !ti.LastFailedAt.After(now.Add(-cc.Dur))
		}
	case "expires":
		bound := cc.Time
		if cc.IsDur {
			bound = now.Add(cc.Dur)
		}
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			if ti.CompletedAt.IsZero() || ti.Retention <= 0 {
				return false
			}
			return !ti.CompletedAt.Add(ti.Retention).After(bound)
		}
	}
	return func(_ *Env, _ *asynq.TaskInfo) bool { return true }
}

func envNow(env *Env, fallback time.Time) time.Time {
	if env != nil && !env.Now.IsZero() {
		return env.Now
	}
	return fallback
}

// clausePredicate builds the per-task predicate for a non-score clause.
// Relative durations anchor to the compile-time now (one clock per request).
func clausePredicate(c *Clause, now time.Time) func(*Env, *asynq.TaskInfo) bool {
	cc := *c
	switch cc.Field {
	case "": // free text — id/type/queue/payload, case-insensitive substring
		needle := strings.ToLower(cc.Value)
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return strings.Contains(strings.ToLower(ti.ID), needle) ||
				strings.Contains(strings.ToLower(ti.Type), needle) ||
				strings.Contains(strings.ToLower(ti.Queue), needle) ||
				strings.Contains(strings.ToLower(string(ti.Payload)), needle)
		}
	case "type":
		if cc.Op == OpEq {
			return func(_ *Env, ti *asynq.TaskInfo) bool { return ti.Type == cc.Value }
		}
		needle := strings.ToLower(cc.Value)
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return strings.Contains(strings.ToLower(ti.Type), needle)
		}
	case "id":
		return func(_ *Env, ti *asynq.TaskInfo) bool { return ti.ID == cc.Value }
	case "payload":
		needle := strings.ToLower(cc.Value)
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return strings.Contains(strings.ToLower(string(ti.Payload)), needle)
		}
	case "meta":
		key, want := cc.MetaKey, cc.Value
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			var obj map[string]interface{}
			if err := json.Unmarshal(ti.Payload, &obj); err != nil {
				return false
			}
			v, ok := obj[key]
			return ok && metaScalarString(v) == want
		}
	case "retries":
		n := int(cc.Num)
		switch cc.Op {
		case OpGe:
			return func(_ *Env, ti *asynq.TaskInfo) bool { return ti.Retried >= n }
		case OpLt:
			return func(_ *Env, ti *asynq.TaskInfo) bool { return ti.Retried < n }
		default: // OpEq
			return func(_ *Env, ti *asynq.TaskInfo) bool { return ti.Retried == n }
		}
	case "error":
		needle := strings.ToLower(cc.Value)
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return ti.LastErr != "" && strings.Contains(strings.ToLower(ti.LastErr), needle)
		}
	case "failed_after":
		bound := cc.Time
		if cc.IsDur {
			bound = now.Add(-cc.Dur)
		}
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return !ti.LastFailedAt.IsZero() && !ti.LastFailedAt.Before(bound)
		}
	case "failed_before":
		bound := cc.Time
		if cc.IsDur {
			bound = now.Add(-cc.Dur)
		}
		return func(_ *Env, ti *asynq.TaskInfo) bool {
			return !ti.LastFailedAt.IsZero() && ti.LastFailedAt.Before(bound)
		}
	case "pending_age":
		d := cc.Dur
		return func(env *Env, ti *asynq.TaskInfo) bool {
			if env == nil || env.PendingSince == nil {
				return false
			}
			since, ok := env.PendingSince[EnvKey(ti.Queue, ti.ID)]
			return ok && envNow(env, now).Sub(since) > d
		}
	case "running":
		d := cc.Dur
		return func(env *Env, ti *asynq.TaskInfo) bool {
			if env == nil || env.Workers == nil {
				return false
			}
			w, ok := env.Workers[EnvKey(ti.Queue, ti.ID)]
			return ok && !w.Started.IsZero() && envNow(env, now).Sub(w.Started) > d
		}
	case "deadline_pct":
		pct := cc.Num
		return func(env *Env, ti *asynq.TaskInfo) bool {
			if env == nil || env.Workers == nil {
				return false
			}
			w, ok := env.Workers[EnvKey(ti.Queue, ti.ID)]
			if !ok || w.Started.IsZero() || w.Deadline.IsZero() || !w.Deadline.After(w.Started) {
				return false
			}
			used := envNow(env, now).Sub(w.Started).Seconds()
			budget := w.Deadline.Sub(w.Started).Seconds()
			return used/budget*100 > pct
		}
	case "orphaned":
		return func(env *Env, ti *asynq.TaskInfo) bool {
			return env != nil && env.Orphans != nil && env.Orphans[EnvKey(ti.Queue, ti.ID)]
		}
	case "group_age":
		d := cc.Dur
		return func(env *Env, ti *asynq.TaskInfo) bool {
			if env == nil || env.GroupEnteredAt == nil {
				return false
			}
			at, ok := env.GroupEnteredAt[EnvKey(ti.Queue, ti.ID)]
			return ok && envNow(env, now).Sub(at) > d
		}
	}
	return func(_ *Env, _ *asynq.TaskInfo) bool { return true }
}

// metaScalarString renders a JSON scalar the way the console chips do;
// nested values render "" and therefore never match. Mirrors the search
// endpoint's semantics exactly.
func metaScalarString(v interface{}) string {
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

// ----------------------------------------------------------------------------
// jobs.Matcher seam
// ----------------------------------------------------------------------------

// CompileMatcher compiles the query into an env-free predicate for the bulk
// job runner's per-task re-verification (§4.3 step 5). Predicates that need
// live env data (running>, deadline_pct>, orphaned, pending_age>,
// group_age>) return a structured error: the runner re-fetches TaskInfo
// only, and pretending to evaluate them would silently act on wrong tasks.
// Relative durations anchor to now — the moment the matcher is built.
func CompileMatcher(q *Query, state string, now time.Time) (func(*asynq.TaskInfo) bool, *ParseError) {
	plan, err := Compile(q, state, now)
	if err != nil {
		return nil, err
	}
	if plan.NeedsWorkers || plan.NeedsOrphans || plan.NeedsPendingSince || plan.NeedsGroupScores {
		offender := ""
		pos := 0
		for _, c := range q.Clauses {
			if c.Field != "" && fieldSpecs[c.Field].env {
				offender, pos = c.Field, c.Pos
				break
			}
		}
		return nil, &ParseError{
			Msg: fmt.Sprintf("`%s` reads live worker/lease data and cannot be re-verified by the job runner", offender),
			Pos: pos,
			Hint: "job scopes support the message-backed fields (queue, type, id, payload~, meta.*, retries, error~, " +
				"next_run, past_due, failed_after/before, died>, expires<, group=); scan-tier support arrives in phase 12",
		}
	}
	env := &Env{Now: now}
	match := plan.Match
	return func(ti *asynq.TaskInfo) bool { return match(env, ti) }, nil
}

// ----------------------------------------------------------------------------
// Cursor encoding — shared by the HTTP layer so the wire format lives beside
// the plan it belongs to.
// ----------------------------------------------------------------------------

// Cursor is a (queue, score, id) resume point for ModeCursor listings.
type Cursor struct {
	Queue string  `json:"q"`
	Score float64 `json:"s"`
	ID    string  `json:"id"`
}

// ScanCursor is a (queue, group, page) resume point for ModeScan listings.
type ScanCursor struct {
	Queue string `json:"q"`
	Group string `json:"g,omitempty"`
	Page  int    `json:"p"`
}

// Encode/Decode use base64url-wrapped JSON — the cursor is opaque to the
// frontend, which only ever echoes it back.
func EncodeCursor(c Cursor) string         { return encodeCursorJSON(c) }
func EncodeScanCursor(c ScanCursor) string { return encodeCursorJSON(c) }

func DecodeCursor(s string) (Cursor, error) {
	var c Cursor
	err := decodeCursorJSON(s, &c)
	return c, err
}

func DecodeScanCursor(s string) (ScanCursor, error) {
	var c ScanCursor
	err := decodeCursorJSON(s, &c)
	return c, err
}

func encodeCursorJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursorJSON(s string, v interface{}) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
