// Package aql implements the Fleet Console task query language (build
// contract §3.4, phase 6): a typed grammar of `field OP value` clauses
// (space = AND) gated per task state to predicates Redis can actually
// answer. Parsing rejects unsupported field/state combinations at parse
// time with an explanation and the nearest supported alternative — the
// console never silently drops a clause and never pretends an unanswerable
// predicate was applied.
//
// Grammar:
//
//	query    := clause (WS clause)*
//	clause   := field op value | flag | freetext
//	field    := queue | type | id | payload | meta.KEY | retries | error
//	          | pending_age | running | deadline_pct | next_run
//	          | failed_after | failed_before | died | expires
//	          | group | group_age | state
//	op       := = | ~ | > | >= | < | <=      (per-field subset, see fieldSpecs)
//	flag     := past_due | orphaned
//	value    := bareword | "quoted string" | duration | RFC3339 | number
//	duration := Go duration (90s, 5m, 2h, 1.5h) with a d suffix for days
//	            (7d, 1d12h)
//	freetext := bareword | "quoted string"   — case-insensitive substring
//	            over id/type/queue/payload (all states, scan-only; kept for
//	            compatibility with the pre-AQL console's free-text search)
//
// Every ParseError carries the byte offset of the offending clause so the
// console can render a caret under it.
package aql

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ****************************************************************************
// This file defines:
//   - the clause/query AST and ParseError (position-carrying)
//   - the lexer + parser
//   - the normative §3.4 field/state matrix and its parse-time enforcement
//     (rejection messages with nearest-alternative hints)
//   - required-state inference
// ****************************************************************************

// States is the canonical task-state vocabulary, in display order.
var States = []string{"pending", "active", "scheduled", "retry", "archived", "completed", "aggregating"}

func isState(s string) bool {
	for _, st := range States {
		if s == st {
			return true
		}
	}
	return false
}

// Op is a clause operator.
type Op string

const (
	OpEq   Op = "="
	OpSub  Op = "~"
	OpGt   Op = ">"
	OpGe   Op = ">="
	OpLt   Op = "<"
	OpLe   Op = "<="
	OpNone Op = "" // flag clauses (past_due, orphaned) and free text
)

// Clause is one parsed predicate.
type Clause struct {
	// Field is the canonical field name ("queue", "meta.<key>", "past_due",
	// …). Empty for free-text clauses.
	Field string
	// MetaKey is the payload key for meta.* clauses.
	MetaKey string
	Op      Op
	// Value is the raw (unquoted) value text. Empty for flag clauses.
	Value string

	// Exactly one of the typed values below is populated, per the field's
	// value kind.
	Num  float64       // retries, deadline_pct
	Dur  time.Duration // duration values (5m/2h/7d)
	Time time.Time     // absolute RFC3339 values
	// IsDur distinguishes Dur from Time for timeOrDur fields.
	IsDur bool

	// Pos/Len locate the whole clause in the input (byte offsets) for caret
	// rendering.
	Pos int
	Len int
}

// Query is a parsed AQL query.
type Query struct {
	Raw     string
	Clauses []Clause
}

// ParseError is a positioned rejection. Its JSON shape ({error, position,
// hint}) is the frontend contract for the 400 body and the inline banner.
type ParseError struct {
	Msg  string `json:"error"`
	Pos  int    `json:"position"`
	Hint string `json:"hint,omitempty"`
}

func (e *ParseError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s (at offset %d) — %s", e.Msg, e.Pos, e.Hint)
	}
	return fmt.Sprintf("%s (at offset %d)", e.Msg, e.Pos)
}

// ----------------------------------------------------------------------------
// The §3.4 field/state matrix (normative)
// ----------------------------------------------------------------------------

type valueKind int

const (
	kindString    valueKind = iota // bareword or quoted string
	kindInt                        // non-negative integer (retries)
	kindPercent                    // 0–100, optional "%" suffix (deadline_pct)
	kindDuration                   // relative duration only (ages)
	kindTimeOrDur                  // duration or absolute RFC3339 (event times)
	kindFlag                       // no value (past_due, orphaned)
)

// fieldSpec is one row of the matrix: which operators the field accepts,
// which states can answer it, what its value looks like, whether evaluating
// it requires decoding the task message (scan) or reading per-task live data
// (env), and the backing-data explanation used in rejection messages.
type fieldSpec struct {
	ops    []Op
	states []string // nil = all seven
	kind   valueKind
	// scan marks predicates that require per-task message decode — they can
	// never be answered by a zset score range alone (§3.4 "msg decode (scan)").
	scan bool
	// env marks predicates that need live data beyond TaskInfo (worker
	// tables, lease scores, pending_since, group scores). Env predicates are
	// scan-only AND unavailable to job-scope matchers (the runner re-fetches
	// TaskInfo only).
	env bool
	// backing explains where the data lives, phrased to complete the
	// sentence "`field` <backing>" in rejection messages.
	backing string
}

var allStates []string // nil sentinel expanded lazily

var fieldSpecs = map[string]fieldSpec{
	"queue":   {ops: []Op{OpEq}, kind: kindString, backing: "matches the queue name"},
	"type":    {ops: []Op{OpEq, OpSub}, kind: kindString, scan: true, backing: "matches the task type (msg decode)"},
	"id":      {ops: []Op{OpEq}, kind: kindString, scan: true, backing: "matches the task id"},
	"payload": {ops: []Op{OpSub}, kind: kindString, scan: true, backing: "substring-matches the raw payload (msg decode)"},
	"meta":    {ops: []Op{OpEq}, kind: kindString, scan: true, backing: "matches a top-level JSON payload key (msg decode)"},
	"retries": {
		ops:    []Op{OpGe, OpLt, OpEq},
		states: []string{"pending", "active", "scheduled", "retry", "archived", "aggregating"},
		kind:   kindInt, scan: true,
		backing: "reads msg.Retried, which asynq does not retain for completed tasks",
	},
	"error": {
		ops:    []Op{OpSub},
		states: []string{"retry", "archived"},
		kind:   kindString, scan: true,
		backing: "matches msg.ErrorMsg (the last error only), which asynq stores only for retry and archived tasks",
	},
	"pending_age": {
		ops:    []Op{OpGt},
		states: []string{"pending"},
		kind:   kindDuration, scan: true, env: true,
		backing: "reads the pending_since hash field, which exists only while a task is pending (asynq deletes it on dequeue)",
	},
	"running": {
		ops:    []Op{OpGt},
		states: []string{"active"},
		kind:   kindDuration, scan: true, env: true,
		backing: "reads live WorkerInfo.Started, which exists only for active tasks",
	},
	"deadline_pct": {
		ops:    []Op{OpGt},
		states: []string{"active"},
		kind:   kindPercent, scan: true, env: true,
		backing: "reads live WorkerInfo.Started/Deadline, which exist only for active tasks",
	},
	"next_run": {
		ops:    []Op{OpLt, OpGt},
		states: []string{"scheduled", "retry"},
		kind:   kindTimeOrDur,
		backing: "reads the zset score (next run time), which exists only for scheduled and retry tasks",
	},
	"past_due": {
		ops:    nil,
		states: []string{"scheduled", "retry"},
		kind:   kindFlag,
		backing: "reads the zset score (next run time), which exists only for scheduled and retry tasks",
	},
	"failed_after": {
		ops:    []Op{OpEq},
		states: []string{"retry", "archived"},
		kind:   kindTimeOrDur, scan: true,
		backing: "reads msg.LastFailedAt, which asynq stores only for retry and archived tasks",
	},
	"failed_before": {
		ops:    []Op{OpEq},
		states: []string{"retry", "archived"},
		kind:   kindTimeOrDur, scan: true,
		backing: "reads msg.LastFailedAt, which asynq stores only for retry and archived tasks",
	},
	"died": {
		ops:    []Op{OpGt},
		states: []string{"archived"},
		kind:   kindDuration,
		backing: "reads the archived zset score (died-at), which exists only for archived tasks",
	},
	"expires": {
		ops:    []Op{OpLt},
		states: []string{"completed"},
		kind:   kindTimeOrDur,
		backing: "reads the completed zset score (expire-at), which exists only for completed tasks",
	},
	"group": {
		ops:    []Op{OpEq},
		states: []string{"aggregating"},
		kind:   kindString,
		backing: "matches the aggregation group, which exists only for aggregating tasks",
	},
	"group_age": {
		ops:    []Op{OpGt},
		states: []string{"aggregating"},
		kind:   kindDuration, scan: true, env: true,
		backing: "reads the group zset score (entered-group-at), which exists only for aggregating tasks",
	},
	"orphaned": {
		ops:    nil,
		states: []string{"active"},
		kind:   kindFlag, scan: true, env: true,
		backing: "compares the lease score against now−grace, which is meaningful only for active tasks",
	},
	// state is the meta-clause: it does not filter task data, it names the
	// state the query enumerates. At most one, and it must agree with every
	// other clause's allowed states.
	"state": {ops: []Op{OpEq}, kind: kindString, backing: "names the task state the query enumerates"},
}

// supportedAgesHint is the §3.4 nearest-alternative catalog, verbatim. It is
// the hint on every unknown time-shaped field and on age-field/state
// mismatches.
const supportedAgesHint = "Supported ages: `pending_age` (pending only), `running>` (active), " +
	"`next_run</past_due` (scheduled/retry), `failed_before/after` (retry/archived, from last_failed_at), " +
	"`died>` (archived), `expires<` (completed). Try `pending_age>2h`."

// enqueuedAliases are unknown fields that specifically mean "when was this
// enqueued" — they get the §3.4 no-enqueue-timestamp rejection verbatim.
var enqueuedAliases = map[string]bool{
	"enqueued": true, "enqueued_at": true, "created": true, "created_at": true,
	"queued": true, "queued_at": true, "age": true,
}

// ageAlternatives maps a state to the age/time predicates it CAN answer, for
// mismatch hints ("In state=pending, try `pending_age>`").
var ageAlternatives = map[string]string{
	"pending":     "`pending_age>`",
	"active":      "`running>` or `deadline_pct>`",
	"scheduled":   "`next_run<`, `next_run>` or `past_due`",
	"retry":       "`next_run<`, `next_run>`, `past_due` or `failed_after/failed_before`",
	"archived":    "`died>` or `failed_after/failed_before`",
	"completed":   "`expires<`",
	"aggregating": "`group_age>`",
}

// timeFields marks the matrix's age/time fields — mismatches on these get
// the supported-ages catalog as their hint.
var timeFields = map[string]bool{
	"pending_age": true, "running": true, "deadline_pct": true, "next_run": true,
	"past_due": true, "failed_after": true, "failed_before": true, "died": true,
	"expires": true, "group_age": true,
}

func (s fieldSpec) allowedIn(state string) bool {
	if s.states == nil {
		return true
	}
	for _, st := range s.states {
		if st == state {
			return true
		}
	}
	return false
}

func (s fieldSpec) statesLabel() string {
	if s.states == nil {
		return "every state"
	}
	return strings.Join(s.states, "/")
}

// ----------------------------------------------------------------------------
// Lexer
// ----------------------------------------------------------------------------

type token struct {
	text string
	pos  int
}

// lex splits the input on whitespace outside double quotes, keeping byte
// offsets. An unterminated quote is a parse error (positioned at the quote).
func lex(input string) ([]token, *ParseError) {
	var toks []token
	i := 0
	for i < len(input) {
		if input[i] == ' ' || input[i] == '\t' || input[i] == '\n' {
			i++
			continue
		}
		start := i
		inQuote := false
		quotePos := -1
		for i < len(input) {
			c := input[i]
			if c == '"' {
				if !inQuote {
					quotePos = i
				}
				inQuote = !inQuote
			} else if (c == ' ' || c == '\t' || c == '\n') && !inQuote {
				break
			}
			i++
		}
		if inQuote {
			return nil, &ParseError{Msg: "unterminated quote", Pos: quotePos,
				Hint: `close the quote: error~"gateway timeout"`}
		}
		toks = append(toks, token{text: input[start:i], pos: start})
	}
	return toks, nil
}

// clauseShapedRE recognizes `field OP …` tokens so callers can distinguish
// an AQL query from pre-AQL free text (IsQuery below).
var clauseShapedRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*(>=|<=|=|~|>|<)`)

// IsQuery reports whether the string contains at least one clause-shaped or
// flag token — i.e. whether it should be parsed as AQL. Plain free text
// (the pre-AQL console's search strings) returns false so legacy URLs keep
// their exact substring semantics.
func IsQuery(input string) bool {
	toks, err := lex(input)
	if err != nil {
		// An unterminated quote only matters if we would parse it; free text
		// with a stray quote stays free text.
		return false
	}
	for _, t := range toks {
		if clauseShapedRE.MatchString(t.text) {
			return true
		}
		if spec, ok := fieldSpecs[t.text]; ok && spec.kind == kindFlag {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// Parser
// ----------------------------------------------------------------------------

// operator scan order: two-character operators first so ">=" is not misread
// as ">" with a value of "=…".
var opTokens = []Op{OpGe, OpLe, OpEq, OpSub, OpGt, OpLt}

// Parse parses an AQL query. It performs full syntactic validation (fields,
// operators, value shapes) and the parse-time state gating of §3.4: every
// clause's allowed-state set is intersected left to right, so a query whose
// clauses cannot all be answered in any single state is rejected at the
// clause that emptied the intersection.
func Parse(input string) (*Query, *ParseError) {
	toks, lerr := lex(input)
	if lerr != nil {
		return nil, lerr
	}
	q := &Query{Raw: input}
	for _, t := range toks {
		c, err := parseClause(t)
		if err != nil {
			return nil, err
		}
		q.Clauses = append(q.Clauses, *c)
	}
	if err := q.gateStates(); err != nil {
		return nil, err
	}
	return q, nil
}

func parseClause(t token) (*Clause, *ParseError) {
	text := t.text

	// Flag clause or bare free text (no operator outside quotes).
	opIdx, op := findOp(text)
	if opIdx < 0 {
		word := unquote(text)
		if spec, ok := fieldSpecs[text]; ok && spec.kind == kindFlag {
			return &Clause{Field: text, Op: OpNone, Pos: t.pos, Len: len(text)}, nil
		}
		if _, ok := fieldSpecs[text]; ok {
			return nil, &ParseError{
				Msg: fmt.Sprintf("`%s` needs an operator and a value", text), Pos: t.pos,
				Hint: fmt.Sprintf("write `%s%s…`", text, string(firstOp(text))),
			}
		}
		// Free text: substring over id/type/queue/payload.
		return &Clause{Field: "", Op: OpNone, Value: word, Pos: t.pos, Len: len(text)}, nil
	}

	field := text[:opIdx]
	rawValue := text[opIdx+len(op):]
	value := unquote(rawValue)
	valuePos := t.pos + opIdx + len(op)

	name := field
	metaKey := ""
	if strings.HasPrefix(field, "meta.") {
		name = "meta"
		metaKey = field[len("meta."):]
		if metaKey == "" {
			return nil, &ParseError{Msg: "`meta.` needs a key", Pos: t.pos, Hint: "write `meta.region=eu`"}
		}
	}

	spec, known := fieldSpecs[name]
	if !known {
		return nil, unknownFieldError(field, value, t.pos)
	}
	if spec.kind == kindFlag {
		return nil, &ParseError{
			Msg: fmt.Sprintf("`%s` is a flag and takes no value", field), Pos: t.pos,
			Hint: fmt.Sprintf("write just `%s`", field),
		}
	}
	if !opAllowed(spec, op) {
		return nil, &ParseError{
			Msg:  fmt.Sprintf("`%s` does not support `%s`", field, op),
			Pos:  t.pos,
			Hint: fmt.Sprintf("supported: %s", opsLabel(field, spec)),
		}
	}
	if value == "" {
		return nil, &ParseError{Msg: fmt.Sprintf("`%s%s` needs a value", field, op), Pos: t.pos}
	}

	c := &Clause{Field: name, MetaKey: metaKey, Op: op, Value: value, Pos: t.pos, Len: len(text)}
	if name == "state" && !isState(value) {
		return nil, &ParseError{
			Msg:  fmt.Sprintf("unknown state %q", value),
			Pos:  valuePos,
			Hint: "one of: " + strings.Join(States, ", "),
		}
	}
	if err := parseValue(c, spec, valuePos); err != nil {
		return nil, err
	}
	return c, nil
}

func findOp(text string) (int, Op) {
	inQuote := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		for _, op := range opTokens {
			if strings.HasPrefix(text[i:], string(op)) {
				if i == 0 {
					return -1, OpNone // no field before the operator: free text
				}
				return i, op
			}
		}
	}
	return -1, OpNone
}

func firstOp(field string) Op {
	if spec, ok := fieldSpecs[field]; ok && len(spec.ops) > 0 {
		return spec.ops[0]
	}
	return OpEq
}

func opAllowed(spec fieldSpec, op Op) bool {
	for _, o := range spec.ops {
		if o == op {
			return true
		}
	}
	return false
}

func opsLabel(field string, spec fieldSpec) string {
	parts := make([]string, len(spec.ops))
	for i, o := range spec.ops {
		parts[i] = fmt.Sprintf("`%s%s`", field, o)
	}
	return strings.Join(parts, ", ")
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// unknownFieldError implements the §3.4 special case: an unknown field whose
// value (or name) is time-shaped gets the no-enqueue-timestamp explanation
// and the supported-ages catalog, verbatim.
func unknownFieldError(field, value string, pos int) *ParseError {
	timeShaped := enqueuedAliases[field]
	if !timeShaped {
		if _, ok := parseDur(strings.TrimPrefix(value, "-")); ok {
			timeShaped = true
		} else if _, err := time.Parse(time.RFC3339, value); err == nil {
			timeShaped = true
		}
	}
	if timeShaped {
		msg := fmt.Sprintf("unknown time field `%s`", field)
		if enqueuedAliases[field] {
			msg = "asynq stores no enqueue timestamp"
		}
		return &ParseError{Msg: msg, Pos: pos, Hint: supportedAgesHint}
	}
	known := make([]string, 0, len(fieldSpecs))
	for name := range fieldSpecs {
		if name == "meta" {
			name = "meta.KEY"
		}
		known = append(known, name)
	}
	sort.Strings(known)
	return &ParseError{
		Msg:  fmt.Sprintf("unknown field `%s`", field),
		Pos:  pos,
		Hint: "known fields: " + strings.Join(known, ", "),
	}
}

// parseDur parses a duration, extending Go's syntax with a leading day
// component ("7d", "1d12h").
func parseDur(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	if i := strings.IndexByte(s, 'd'); i > 0 {
		days, err := strconv.Atoi(s[:i])
		if err != nil || days < 0 {
			return 0, false
		}
		rest := time.Duration(0)
		if i+1 < len(s) {
			var perr error
			rest, perr = time.ParseDuration(s[i+1:])
			if perr != nil || rest < 0 {
				return 0, false
			}
		}
		return time.Duration(days)*24*time.Hour + rest, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

func parseValue(c *Clause, spec fieldSpec, valuePos int) *ParseError {
	switch spec.kind {
	case kindString:
		return nil
	case kindInt:
		n, err := strconv.Atoi(c.Value)
		if err != nil || n < 0 {
			return &ParseError{Msg: fmt.Sprintf("`%s` needs a non-negative integer, got %q", c.Field, c.Value), Pos: valuePos}
		}
		c.Num = float64(n)
		return nil
	case kindPercent:
		v := strings.TrimSuffix(c.Value, "%")
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 || n > 100 {
			return &ParseError{Msg: fmt.Sprintf("`%s` needs a percentage 0–100, got %q", c.Field, c.Value), Pos: valuePos}
		}
		c.Num = n
		return nil
	case kindDuration:
		d, ok := parseDur(c.Value)
		if !ok {
			return &ParseError{
				Msg:  fmt.Sprintf("`%s` needs a duration (5m, 2h, 7d), got %q", c.Field, c.Value),
				Pos:  valuePos,
				Hint: fmt.Sprintf("write `%s%s2h`", c.Field, c.Op),
			}
		}
		c.Dur, c.IsDur = d, true
		return nil
	case kindTimeOrDur:
		if d, ok := parseDur(c.Value); ok {
			c.Dur, c.IsDur = d, true
			return nil
		}
		if ts, err := time.Parse(time.RFC3339, c.Value); err == nil {
			c.Time = ts
			return nil
		}
		return &ParseError{
			Msg: fmt.Sprintf("`%s` needs a duration (5m, 2h, 7d) or an RFC3339 time, got %q", c.Field, c.Value),
			Pos: valuePos,
			Hint: fmt.Sprintf("write `%s%s2h` or `%s%s2026-07-25T00:00:00Z`", c.Field, c.Op, c.Field, c.Op),
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// State gating & inference
// ----------------------------------------------------------------------------

// gateStates intersects every clause's allowed-state set left to right and
// rejects at the clause that empties it — parse-time enforcement of the
// matrix, including explicit `state=` clauses wherever they appear.
func (q *Query) gateStates() *ParseError {
	allowed := append([]string(nil), States...)
	for i := range q.Clauses {
		c := &q.Clauses[i]
		var clauseStates []string
		switch {
		case c.Field == "":
			continue // free text: all states
		case c.Field == "state":
			clauseStates = []string{c.Value}
		default:
			spec := fieldSpecs[c.Field]
			if spec.states == nil {
				continue
			}
			clauseStates = spec.states
		}
		next := intersect(allowed, clauseStates)
		if len(next) == 0 {
			return q.mismatchError(c, allowed)
		}
		allowed = next
	}
	return nil
}

// mismatchError builds the §3.4 rejection: why the field cannot be answered
// given the states earlier clauses narrowed the query to, where it IS
// supported, and the nearest supported alternative.
func (q *Query) mismatchError(c *Clause, narrowedTo []string) *ParseError {
	display := c.Field
	if c.Field == "meta" {
		display = "meta." + c.MetaKey
	}
	if c.Field == "state" {
		// state=X after clauses that exclude X: blame the combination.
		return &ParseError{
			Msg: fmt.Sprintf("state=%s conflicts with earlier clauses, which are only answerable for %s tasks",
				c.Value, strings.Join(narrowedTo, "/")),
			Pos:  c.Pos,
			Hint: fmt.Sprintf("drop state=%s or the conflicting clause", c.Value),
		}
	}
	spec := fieldSpecs[c.Field]
	msg := fmt.Sprintf("`%s` %s — not answerable for %s tasks",
		display, spec.backing, strings.Join(narrowedTo, "/"))
	hint := fmt.Sprintf("`%s` works in: %s.", display, spec.statesLabel())
	if timeFields[c.Field] {
		if len(narrowedTo) == 1 {
			if alt, ok := ageAlternatives[narrowedTo[0]]; ok {
				hint += fmt.Sprintf(" In state=%s, try %s.", narrowedTo[0], alt)
			}
		} else {
			hint += " " + supportedAgesHint
		}
	}
	return &ParseError{Msg: msg, Pos: c.Pos, Hint: hint}
}

func intersect(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// ExplicitState returns the state named by a `state=` clause, if any.
func (q *Query) ExplicitState() (string, bool) {
	for _, c := range q.Clauses {
		if c.Field == "state" {
			return c.Value, true
		}
	}
	return "", false
}

// AllowedStates returns the states every clause can be answered in — the
// left-to-right intersection gateStates already proved non-empty.
func (q *Query) AllowedStates() []string {
	allowed := append([]string(nil), States...)
	for _, c := range q.Clauses {
		switch {
		case c.Field == "":
			continue
		case c.Field == "state":
			allowed = intersect(allowed, []string{c.Value})
		default:
			if spec := fieldSpecs[c.Field]; spec.states != nil {
				allowed = intersect(allowed, spec.states)
			}
		}
	}
	return allowed
}

// ResolveState picks the state the query enumerates: an explicit `state=`
// clause wins; then the caller's state (the legacy `state` param or the
// console's active pill) if the query allows it; then a uniquely-inferred
// state; then "pending" when still allowed. A caller state the query cannot
// answer is a positioned rejection, not a silent override.
func (q *Query) ResolveState(callerState string) (string, *ParseError) {
	if st, ok := q.ExplicitState(); ok {
		if callerState != "" && callerState != st {
			// Not an error: the explicit clause is the single source of truth;
			// the caller param is legacy context.
			return st, nil
		}
		return st, nil
	}
	allowed := q.AllowedStates()
	if callerState != "" {
		for _, s := range allowed {
			if s == callerState {
				return callerState, nil
			}
		}
		// Find the first clause that excludes the caller state for a
		// positioned rejection.
		for i := range q.Clauses {
			c := &q.Clauses[i]
			if c.Field == "" || c.Field == "state" {
				continue
			}
			if spec := fieldSpecs[c.Field]; spec.states != nil && !spec.allowedIn(callerState) {
				return "", q.mismatchError(c, []string{callerState})
			}
		}
		return "", &ParseError{
			Msg:  fmt.Sprintf("query is not answerable for state=%s", callerState),
			Pos:  0,
			Hint: "answerable in: " + strings.Join(allowed, ", "),
		}
	}
	if len(allowed) == 1 {
		return allowed[0], nil
	}
	for _, s := range allowed {
		if s == "pending" {
			return "pending", nil
		}
	}
	return allowed[0], nil
}

// QueueFilter returns the queue named by a `queue=` clause ("" = all).
func (q *Query) QueueFilter() string {
	for _, c := range q.Clauses {
		if c.Field == "queue" {
			return c.Value
		}
	}
	return ""
}
