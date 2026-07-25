package stats

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ****************************************************************************
// This file defines:
//   - server-side sorting over cached snapshots (tier 1: full in-process sort)
//   - the v1 queue filter: lenient, substring + numeric comparisons
// ****************************************************************************

// SortColumn names a sortable directory column. String values double as the
// HTTP `sort` parameter contract.
type SortColumn string

const (
	SortName             SortColumn = "name"
	SortPending          SortColumn = "pending"
	SortActive           SortColumn = "active"
	SortScheduled        SortColumn = "scheduled"
	SortRetry            SortColumn = "retry"
	SortArchived         SortColumn = "archived"
	SortCompleted        SortColumn = "completed"
	SortOldestPendingAge SortColumn = "oldest_pending_age"
	// SortLatency is an alias of SortOldestPendingAge: asynq's QueueInfo
	// "latency" IS the age of the oldest pending task, and pretending they
	// are different metrics would be dishonest. Both names are accepted
	// because both appear as columns in the directory spec (§3.2).
	SortLatency        SortColumn = "latency"
	SortProcessedToday SortColumn = "processed_today"
	SortFailedToday    SortColumn = "failed_today"
	SortErrorRate      SortColumn = "error_rate"
	SortConsumers      SortColumn = "consumers"
)

// ParseSortColumn validates an HTTP sort parameter. Empty means SortName.
func ParseSortColumn(s string) (SortColumn, error) {
	switch SortColumn(s) {
	case "":
		return SortName, nil
	case SortName, SortPending, SortActive, SortScheduled, SortRetry, SortArchived,
		SortCompleted, SortOldestPendingAge, SortLatency, SortProcessedToday,
		SortFailedToday, SortErrorRate, SortConsumers:
		return SortColumn(s), nil
	}
	return "", fmt.Errorf("unsupported sort column %q", s)
}

// sortValue maps a snapshot to the numeric value a column sorts on. Ages are
// computed against a single `now` for the whole sort so ordering is
// consistent within one request. float64 is exact for every count that fits
// in 2^53 — beyond any per-queue count Redis can hold in practice.
func sortValue(s *QueueSnapshot, col SortColumn, now time.Time) float64 {
	switch col {
	case SortPending:
		return float64(s.Pending)
	case SortActive:
		return float64(s.Active)
	case SortScheduled:
		return float64(s.Scheduled)
	case SortRetry:
		return float64(s.Retry)
	case SortArchived:
		return float64(s.Archived)
	case SortCompleted:
		return float64(s.Completed)
	case SortOldestPendingAge, SortLatency:
		return float64(s.OldestPendingAge(now))
	case SortProcessedToday:
		return float64(s.ProcessedToday)
	case SortFailedToday:
		return float64(s.FailedToday)
	case SortErrorRate:
		return s.ErrorRate()
	case SortConsumers:
		return float64(s.Consumers)
	}
	return 0
}

// SortSnapshots sorts snaps in place. Tier-1 design decision (§5.1): sorting
// runs in-process over the full cached set on every request — at the tier-1
// ceiling of ~2k queues that is microseconds, and it spares Redis a per-column
// sorted index. Phase 12 revisits this with cached sorted indexes when
// rotating shards raise the ceiling. Ties (and SortName itself) order by
// queue name ascending so pagination is stable across requests.
func SortSnapshots(snaps []*QueueSnapshot, col SortColumn, desc bool, now time.Time) {
	sort.SliceStable(snaps, func(i, j int) bool {
		if col != SortName {
			vi, vj := sortValue(snaps[i], col, now), sortValue(snaps[j], col, now)
			if vi != vj {
				if desc {
					return vi > vj
				}
				return vi < vj
			}
			return snaps[i].Queue < snaps[j].Queue // stable tiebreak, always asc
		}
		if desc {
			return snaps[i].Queue > snaps[j].Queue
		}
		return snaps[i].Queue < snaps[j].Queue
	})
}

// ----------------------------------------------------------------------------
// Filter v1. This is NOT AQL — the full grammar with per-state gating arrives
// in phase 6 and supersedes this. See the grammar comment on
// newFleetQueuesHandlerFunc (stats_handlers.go) for the normative v1 syntax.
// Parsing is deliberately lenient: tokens the parser does not understand are
// collected in Ignored (surfaced to the caller for honesty) instead of
// failing the request.
// ----------------------------------------------------------------------------

type cmpOp int

const (
	opEq cmpOp = iota
	opNe
	opGt
	opGe
	opLt
	opLe
)

func (op cmpOp) holds(cmp int) bool {
	// cmp is -1/0/+1 of (lhs - rhs).
	switch op {
	case opEq:
		return cmp == 0
	case opNe:
		return cmp != 0
	case opGt:
		return cmp > 0
	case opGe:
		return cmp >= 0
	case opLt:
		return cmp < 0
	case opLe:
		return cmp <= 0
	}
	return false
}

type clauseKind int

const (
	kindNameSub clauseKind = iota // substring match on queue name
	kindNameEq                    // exact match on queue name
	kindNum                       // numeric comparison on a snapshot field
	kindPaused                    // paused=true/false
)

type clause struct {
	kind  clauseKind
	op    cmpOp
	field func(*QueueSnapshot, time.Time) float64
	num   float64
	str   string // lowercased needle for name matches
	want  bool   // for kindPaused
}

// Filter is a parsed v1 queue filter: every clause must match (AND).
type Filter struct {
	clauses []clause
	// Ignored lists tokens the lenient parser could not understand. Callers
	// surface these so a typo silently matching everything is visible.
	Ignored []string
}

// Empty reports whether the filter has no effective clauses.
func (f Filter) Empty() bool { return len(f.clauses) == 0 }

// Match reports whether s satisfies every clause.
func (f Filter) Match(s *QueueSnapshot, now time.Time) bool {
	for _, c := range f.clauses {
		if !c.match(s, now) {
			return false
		}
	}
	return true
}

func (c clause) match(s *QueueSnapshot, now time.Time) bool {
	switch c.kind {
	case kindNameSub:
		return strings.Contains(strings.ToLower(s.Queue), c.str)
	case kindNameEq:
		return strings.EqualFold(s.Queue, c.str)
	case kindPaused:
		return s.Paused == c.want
	case kindNum:
		v := c.field(s, now)
		cmp := 0
		if v < c.num {
			cmp = -1
		} else if v > c.num {
			cmp = 1
		}
		return c.op.holds(cmp)
	}
	return false
}

// numField describes a filterable numeric field.
type numField struct {
	value func(*QueueSnapshot, time.Time) float64
	// age fields accept duration values ("30m"); bare numbers mean seconds.
	isAge bool
	// rate fields accept a "%" suffix (error_rate>5%).
	isRate bool
}

// filterFields maps filter field names (and their aliases) to accessors.
var filterFields = map[string]numField{
	"pending":            {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Pending) }},
	"active":             {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Active) }},
	"scheduled":          {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Scheduled) }},
	"retry":              {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Retry) }},
	"archived":           {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Archived) }},
	"completed":          {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Completed) }},
	"groups":             {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Groups) }},
	"consumers":          {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.Consumers) }},
	"processed_today":    {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.ProcessedToday) }},
	"failed_today":       {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.FailedToday) }},
	"orphans":            {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.OrphanCandidates) }},
	"orphan_candidates":  {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.OrphanCandidates) }},
	"past_due":           {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.PastDueScheduled) }},
	"past_due_scheduled": {value: func(s *QueueSnapshot, _ time.Time) float64 { return float64(s.PastDueScheduled) }},
	"error_rate":         {value: func(s *QueueSnapshot, _ time.Time) float64 { return s.ErrorRate() }, isRate: true},
	"oldest_pending_age": {value: func(s *QueueSnapshot, now time.Time) float64 { return s.OldestPendingAge(now).Seconds() }, isAge: true},
	"latency":            {value: func(s *QueueSnapshot, now time.Time) float64 { return s.OldestPendingAge(now).Seconds() }, isAge: true},
}

// operators in scan order: two-character operators first so ">=" is not
// misread as ">" with a value of "=5".
var operators = []struct {
	tok string
	op  cmpOp
}{
	{"!=", opNe}, {">=", opGe}, {"<=", opLe}, {">", opGt}, {"<", opLt}, {"=", opEq},
}

// ParseFilter parses a v1 filter string. It never fails: valid clauses are
// collected, everything else lands in Ignored.
func ParseFilter(query string) Filter {
	var f Filter
	for _, tok := range strings.Fields(query) {
		if c, ok := parseToken(tok); ok {
			f.clauses = append(f.clauses, c)
		} else {
			f.Ignored = append(f.Ignored, tok)
		}
	}
	return f
}

func parseToken(tok string) (clause, bool) {
	// name~sub — explicit substring match.
	if i := strings.Index(tok, "~"); i >= 0 {
		field, val := tok[:i], tok[i+1:]
		if field == "name" && val != "" {
			return clause{kind: kindNameSub, str: strings.ToLower(val)}, true
		}
		return clause{}, false
	}
	// field <op> value.
	for _, o := range operators {
		i := strings.Index(tok, o.tok)
		if i <= 0 || i+len(o.tok) >= len(tok) {
			continue
		}
		field := strings.ToLower(tok[:i])
		val := tok[i+len(o.tok):]
		return parseComparison(field, o.op, val)
	}
	// Bare word: substring on queue name. An operator-shaped token that
	// failed above never reaches this fallback — treating "pending>x" as a
	// name search would silently match nothing the user meant.
	if strings.ContainsAny(tok, "=<>!") {
		return clause{}, false
	}
	return clause{kind: kindNameSub, str: strings.ToLower(tok)}, true
}

func parseComparison(field string, op cmpOp, val string) (clause, bool) {
	switch field {
	case "name":
		if op == opEq {
			return clause{kind: kindNameEq, str: val}, true
		}
		return clause{}, false
	case "paused":
		if op != opEq && op != opNe {
			return clause{}, false
		}
		b, err := strconv.ParseBool(val)
		if err != nil {
			return clause{}, false
		}
		if op == opNe {
			b = !b
		}
		return clause{kind: kindPaused, want: b}, true
	}
	nf, ok := filterFields[field]
	if !ok {
		return clause{}, false
	}
	num, ok := parseNumericValue(val, nf)
	if !ok {
		return clause{}, false
	}
	return clause{kind: kindNum, op: op, field: nf.value, num: num}, true
}

func parseNumericValue(val string, nf numField) (float64, bool) {
	if nf.isRate && strings.HasSuffix(val, "%") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64)
		if err != nil {
			return 0, false
		}
		return n / 100, true
	}
	if n, err := strconv.ParseFloat(val, 64); err == nil {
		// Bare numbers on age fields mean seconds (accessors yield seconds).
		return n, true
	}
	if nf.isAge {
		if d, err := time.ParseDuration(val); err == nil {
			return d.Seconds(), true
		}
	}
	return 0, false
}
