package aql

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Pure-logic tests for the AQL grammar: the full §3.4 field/state matrix
// (accept + reject per cell), value parsing, positioned rejections (incl.
// the verbatim enqueued< hint), state inference, compile modes, predicates,
// and the jobs.Matcher seam. No Redis: the root package's integration suite
// covers cursor listing and progressive scans against real keys.
// ****************************************************************************

// matrixRow is one row of the normative §3.4 matrix: a representative
// clause for the field and the exact set of states that can answer it.
type matrixRow struct {
	field   string
	clause  string
	allowed []string
}

var all7 = []string{"pending", "active", "scheduled", "retry", "archived", "completed", "aggregating"}

var matrix = []matrixRow{
	{"queue", "queue=email", all7},
	{"type~", "type~send", all7},
	{"type=", "type=email:send", all7},
	{"id", "id=abc-123", all7},
	{"payload~", `payload~"user_id"`, all7},
	{"meta.*", "meta.region=eu", all7},
	{"retries", "retries>=3", []string{"pending", "active", "scheduled", "retry", "archived", "aggregating"}},
	{"error~", "error~timeout", []string{"retry", "archived"}},
	{"pending_age", "pending_age>2h", []string{"pending"}},
	{"running", "running>5m", []string{"active"}},
	{"deadline_pct", "deadline_pct>90", []string{"active"}},
	{"next_run<", "next_run<30m", []string{"scheduled", "retry"}},
	{"next_run>", "next_run>2h", []string{"scheduled", "retry"}},
	{"past_due", "past_due", []string{"scheduled", "retry"}},
	{"failed_after", "failed_after=2h", []string{"retry", "archived"}},
	{"failed_before", "failed_before=7d", []string{"retry", "archived"}},
	{"died", "died>3d", []string{"archived"}},
	{"expires", "expires<1h", []string{"completed"}},
	{"group", "group=g1", []string{"aggregating"}},
	{"group_age", "group_age>10m", []string{"aggregating"}},
	{"orphaned", "orphaned", []string{"active"}},
}

func allowedIn(row matrixRow, state string) bool {
	for _, s := range row.allowed {
		if s == state {
			return true
		}
	}
	return false
}

func TestMatrixFieldStateGating(t *testing.T) {
	Convey("Given the normative §3.4 field/state matrix", t, func() {
		for _, row := range matrix {
			row := row
			for _, state := range all7 {
				state := state
				query := row.clause + " state=" + state
				if allowedIn(row, state) {
					Convey(fmt.Sprintf("When %s is combined with state=%s, Then it parses (matrix ✓)", row.field, state), func() {
						q, err := Parse(query)
						So(err, ShouldBeNil)
						So(q, ShouldNotBeNil)

						st, rerr := q.ResolveState("")
						So(rerr, ShouldBeNil)
						So(st, ShouldEqual, state)

						_, cerr := Compile(q, state, time.Now())
						So(cerr, ShouldBeNil)
					})
				} else {
					Convey(fmt.Sprintf("When %s is combined with state=%s, Then it is rejected at parse time (matrix —)", row.field, state), func() {
						_, err := Parse(query)
						So(err, ShouldNotBeNil)
						// The rejection is positioned at a real clause offset.
						So(err.Pos, ShouldBeGreaterThanOrEqualTo, 0)
						So(err.Pos, ShouldBeLessThan, len(query))
						// And names where the field IS supported (nearest
						// alternative style) or the conflict.
						So(err.Msg+err.Hint, ShouldNotBeEmpty)
					})
				}
			}
		}
	})
}

func TestRejectionMessages(t *testing.T) {
	Convey("Given the §3.4 rejection-message contract", t, func() {
		Convey("When an enqueue-timestamp field is used (the verbatim example)", func() {
			_, err := Parse("enqueued<-2h")

			Convey("Then the rejection is the no-enqueue-timestamp explanation with the supported-ages catalog", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldEqual, "asynq stores no enqueue timestamp")
				So(err.Hint, ShouldContainSubstring, "`pending_age` (pending only)")
				So(err.Hint, ShouldContainSubstring, "`running>` (active)")
				So(err.Hint, ShouldContainSubstring, "`next_run</past_due` (scheduled/retry)")
				So(err.Hint, ShouldContainSubstring, "`failed_before/after` (retry/archived, from last_failed_at)")
				So(err.Hint, ShouldContainSubstring, "`died>` (archived)")
				So(err.Hint, ShouldContainSubstring, "`expires<` (completed)")
				So(err.Hint, ShouldContainSubstring, "Try `pending_age>2h`.")
				So(err.Pos, ShouldEqual, 0)
			})
		})

		Convey("When an unknown field carries a duration value", func() {
			_, err := Parse("queue=email oldness>2h")

			Convey("Then it gets the supported-ages catalog and a position at the clause", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "unknown time field `oldness`")
				So(err.Hint, ShouldContainSubstring, "Supported ages:")
				So(err.Pos, ShouldEqual, len("queue=email "))
			})
		})

		Convey("When an unknown non-time field is used", func() {
			_, err := Parse("colour=red")

			Convey("Then the hint lists the known fields", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "unknown field `colour`")
				So(err.Hint, ShouldContainSubstring, "meta.KEY")
				So(err.Hint, ShouldContainSubstring, "pending_age")
			})
		})

		Convey("When an age field is used in the wrong single state", func() {
			_, err := Parse("state=pending died>3d")

			Convey("Then the hint names the state's own age alternative", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "`died`")
				So(err.Hint, ShouldContainSubstring, "`died` works in: archived")
				So(err.Hint, ShouldContainSubstring, "In state=pending, try `pending_age>`")
			})
		})

		Convey("When two clauses cannot share any state", func() {
			_, err := Parse("pending_age>2h died>3d")

			Convey("Then the second clause is rejected with the narrowed states named", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "`died`")
				So(err.Msg, ShouldContainSubstring, "pending")
				So(err.Pos, ShouldEqual, len("pending_age>2h "))
			})
		})

		Convey("When state= itself conflicts with an earlier clause", func() {
			_, err := Parse("died>3d state=pending")

			Convey("Then the state clause is blamed with a drop-one hint", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "state=pending conflicts")
				So(err.Pos, ShouldEqual, len("died>3d "))
			})
		})
	})
}

func TestValueParsing(t *testing.T) {
	Convey("Given AQL value forms", t, func() {
		Convey("When durations use d-days syntax", func() {
			q, err := Parse("died>7d state=archived")
			So(err, ShouldBeNil)

			Convey("Then 7d is 168h", func() {
				So(q.Clauses[0].Dur, ShouldEqual, 7*24*time.Hour)
			})
		})

		Convey("When durations combine days and hours", func() {
			q, err := Parse("died>1d12h state=archived")
			So(err, ShouldBeNil)

			Convey("Then 1d12h is 36h", func() {
				So(q.Clauses[0].Dur, ShouldEqual, 36*time.Hour)
			})
		})

		Convey("When an event-time field takes an absolute RFC3339 value", func() {
			q, err := Parse("failed_after=2026-07-01T00:00:00Z state=archived")
			So(err, ShouldBeNil)

			Convey("Then the absolute time is parsed", func() {
				So(q.Clauses[0].IsDur, ShouldBeFalse)
				So(q.Clauses[0].Time.Year(), ShouldEqual, 2026)
			})
		})

		Convey("When a quoted value contains spaces", func() {
			q, err := Parse(`error~"gateway timeout" state=retry`)
			So(err, ShouldBeNil)

			Convey("Then it stays one clause with the unquoted value", func() {
				So(len(q.Clauses), ShouldEqual, 2)
				So(q.Clauses[0].Value, ShouldEqual, "gateway timeout")
			})
		})

		Convey("When a quote is unterminated", func() {
			_, err := Parse(`error~"gateway timeout state=retry`)

			Convey("Then the error points at the quote", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldEqual, "unterminated quote")
				So(err.Pos, ShouldEqual, len("error~"))
			})
		})

		Convey("When a duration field gets a non-duration", func() {
			_, err := Parse("pending_age>soon")

			Convey("Then the value is rejected with an example", func() {
				So(err, ShouldNotBeNil)
				So(err.Msg, ShouldContainSubstring, "needs a duration")
			})
		})

		Convey("When deadline_pct exceeds 100", func() {
			_, err := Parse("deadline_pct>140")
			So(err, ShouldNotBeNil)
		})

		Convey("When retries gets a negative value", func() {
			_, err := Parse("retries>=-1")
			So(err, ShouldNotBeNil)
		})

		Convey("When an unsupported operator is used", func() {
			_, err := Parse("queue~email")

			Convey("Then the supported operators are listed", func() {
				So(err, ShouldNotBeNil)
				So(err.Hint, ShouldContainSubstring, "`queue=`")
			})
		})

		Convey("When a flag is given a value", func() {
			_, err := Parse("past_due=true")
			So(err, ShouldNotBeNil)
			So(err.Hint, ShouldContainSubstring, "write just `past_due`")
		})

		Convey("When a known field has no operator", func() {
			_, err := Parse("retries")
			So(err, ShouldNotBeNil)
			So(err.Msg, ShouldContainSubstring, "needs an operator")
		})

		Convey("When meta. has no key", func() {
			_, err := Parse("meta.=x")
			So(err, ShouldNotBeNil)
		})
	})
}

func TestStateInference(t *testing.T) {
	Convey("Given required-state inference", t, func() {
		Convey("When a query's fields fit exactly one state", func() {
			q, err := Parse("died>3d")
			So(err, ShouldBeNil)

			Convey("Then the state is inferred without an explicit state=", func() {
				st, rerr := q.ResolveState("")
				So(rerr, ShouldBeNil)
				So(st, ShouldEqual, "archived")
			})
		})

		Convey("When fields fit several states and the caller supplies one of them", func() {
			q, err := Parse("error~timeout")
			So(err, ShouldBeNil)

			Convey("Then the caller state wins", func() {
				st, rerr := q.ResolveState("archived")
				So(rerr, ShouldBeNil)
				So(st, ShouldEqual, "archived")
			})
		})

		Convey("When the caller state cannot answer the query", func() {
			q, err := Parse("error~timeout")
			So(err, ShouldBeNil)

			Convey("Then ResolveState rejects with a positioned error", func() {
				_, rerr := q.ResolveState("pending")
				So(rerr, ShouldNotBeNil)
				So(rerr.Msg, ShouldContainSubstring, "`error`")
			})
		})

		Convey("When nothing narrows the state", func() {
			q, err := Parse("queue=email type~send")
			So(err, ShouldBeNil)

			Convey("Then the default is pending", func() {
				st, rerr := q.ResolveState("")
				So(rerr, ShouldBeNil)
				So(st, ShouldEqual, "pending")
			})
		})

		Convey("When an explicit state= is present", func() {
			q, err := Parse("queue=email state=retry")
			So(err, ShouldBeNil)

			Convey("Then it wins over the caller's state", func() {
				st, rerr := q.ResolveState("pending")
				So(rerr, ShouldBeNil)
				So(st, ShouldEqual, "retry")
			})
		})
	})
}

func TestCompileModes(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given plan compilation", t, func() {
		Convey("When every clause is queue/state/score-backed on a zset state", func() {
			q, err := Parse("queue=email state=scheduled next_run<30m")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "scheduled", now)
			So(cerr, ShouldBeNil)

			Convey("Then the plan is cursorable with the folded score range", func() {
				So(plan.Mode, ShouldEqual, ModeCursor)
				So(plan.Queue, ShouldEqual, "email")
				So(plan.ScoreMax, ShouldEqual, float64(now.Add(30*time.Minute).Unix()))
			})
		})

		Convey("When past_due compiles for scheduled", func() {
			q, err := Parse("state=scheduled past_due")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "scheduled", now)
			So(cerr, ShouldBeNil)

			Convey("Then the range tops out at now", func() {
				So(plan.Mode, ShouldEqual, ModeCursor)
				So(plan.ScoreMax, ShouldEqual, float64(now.Unix()))
			})
		})

		Convey("When died> compiles for archived", func() {
			q, err := Parse("died>3d")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "archived", now)
			So(cerr, ShouldBeNil)

			Convey("Then the range tops out at now−3d", func() {
				So(plan.Mode, ShouldEqual, ModeCursor)
				So(plan.ScoreMax, ShouldEqual, float64(now.Add(-3*24*time.Hour).Unix()))
			})
		})

		Convey("When expires< compiles for completed", func() {
			q, err := Parse("expires<1h")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "completed", now)
			So(cerr, ShouldBeNil)
			So(plan.Mode, ShouldEqual, ModeCursor)
			So(plan.ScoreMax, ShouldEqual, float64(now.Add(time.Hour).Unix()))
		})

		Convey("When a zset state carries no score clause at all", func() {
			q, err := Parse("queue=email state=retry")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "retry", now)
			So(cerr, ShouldBeNil)

			Convey("Then it is still cursorable over the full range (§5.3)", func() {
				So(plan.Mode, ShouldEqual, ModeCursor)
			})
		})

		Convey("When any decode field joins a score clause", func() {
			q, err := Parse("state=retry next_run<30m error~timeout")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "retry", now)
			So(cerr, ShouldBeNil)

			Convey("Then the plan degrades to a scan", func() {
				So(plan.Mode, ShouldEqual, ModeScan)
			})
		})

		Convey("When the state is LIST-backed", func() {
			q, err := Parse("queue=email state=pending")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "pending", now)
			So(cerr, ShouldBeNil)

			Convey("Then the plan is a scan (no zset score to cursor on)", func() {
				So(plan.Mode, ShouldEqual, ModeScan)
			})
		})

		Convey("When env-backed fields compile", func() {
			q, err := Parse("state=active running>5m orphaned")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "active", now)
			So(cerr, ShouldBeNil)

			Convey("Then the plan flags its env needs", func() {
				So(plan.Mode, ShouldEqual, ModeScan)
				So(plan.NeedsWorkers, ShouldBeTrue)
				So(plan.NeedsOrphans, ShouldBeTrue)
			})
		})
	})
}

func testTask(queue, typ, id string, state asynq.TaskState, payload string) *asynq.TaskInfo {
	return &asynq.TaskInfo{ID: id, Queue: queue, Type: typ, State: state, Payload: []byte(payload)}
}

func TestPredicates(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	Convey("Given compiled scan predicates", t, func() {
		env := &Env{Now: now}

		Convey("When matching type/payload/meta/free-text on a pending task", func() {
			q, err := Parse(`queue=email type~send meta.region=eu "gateway"`)
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "pending", now)
			So(cerr, ShouldBeNil)

			match := testTask("email", "email:send", "id-1", asynq.TaskStatePending,
				`{"region":"eu","note":"gateway timeout"}`)
			wrongMeta := testTask("email", "email:send", "id-2", asynq.TaskStatePending,
				`{"region":"us","note":"gateway timeout"}`)
			wrongState := testTask("email", "email:send", "id-3", asynq.TaskStateRetry,
				`{"region":"eu","note":"gateway timeout"}`)

			Convey("Then only the fully-matching pending task passes", func() {
				So(plan.Match(env, match), ShouldBeTrue)
				So(plan.Match(env, wrongMeta), ShouldBeFalse)
				So(plan.Match(env, wrongState), ShouldBeFalse)
			})
		})

		Convey("When matching retries and error on retry tasks", func() {
			q, err := Parse("state=retry retries>=3 error~timeout")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "retry", now)
			So(cerr, ShouldBeNil)

			hot := testTask("q", "t", "a", asynq.TaskStateRetry, "{}")
			hot.Retried, hot.LastErr = 5, "gateway timeout: upstream"
			cold := testTask("q", "t", "b", asynq.TaskStateRetry, "{}")
			cold.Retried, cold.LastErr = 1, "gateway timeout: upstream"

			Convey("Then the retry threshold gates the match", func() {
				So(plan.Match(env, hot), ShouldBeTrue)
				So(plan.Match(env, cold), ShouldBeFalse)
			})
		})

		Convey("When matching failed_after/failed_before", func() {
			q, err := Parse("state=archived failed_after=2h")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "archived", now)
			So(cerr, ShouldBeNil)

			recent := testTask("q", "t", "a", asynq.TaskStateArchived, "{}")
			recent.LastFailedAt = now.Add(-time.Hour)
			old := testTask("q", "t", "b", asynq.TaskStateArchived, "{}")
			old.LastFailedAt = now.Add(-3 * time.Hour)

			Convey("Then only failures within the window match", func() {
				So(plan.Match(env, recent), ShouldBeTrue)
				So(plan.Match(env, old), ShouldBeFalse)
			})
		})

		Convey("When matching env-backed predicates", func() {
			q, err := Parse("state=active running>5m")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "active", now)
			So(cerr, ShouldBeNil)

			ti := testTask("q", "t", "a", asynq.TaskStateActive, "{}")

			Convey("Then a task with worker data older than the bound matches", func() {
				withData := &Env{Now: now, Workers: map[string]WorkerObs{
					EnvKey("q", "a"): {Started: now.Add(-10 * time.Minute)},
				}}
				So(plan.Match(withData, ti), ShouldBeTrue)
			})

			Convey("Then a task with no worker data reports false, never a guess", func() {
				So(plan.Match(&Env{Now: now}, ti), ShouldBeFalse)
			})
		})

		Convey("When matching orphaned", func() {
			q, err := Parse("state=active orphaned")
			So(err, ShouldBeNil)
			plan, cerr := Compile(q, "active", now)
			So(cerr, ShouldBeNil)

			ti := testTask("q", "t", "a", asynq.TaskStateActive, "{}")
			orphanEnv := &Env{Now: now, Orphans: map[string]bool{EnvKey("q", "a"): true}}

			So(plan.Match(orphanEnv, ti), ShouldBeTrue)
			So(plan.Match(&Env{Now: now}, ti), ShouldBeFalse)
		})
	})
}

func TestCompileMatcherSeam(t *testing.T) {
	now := time.Now()

	Convey("Given the jobs.Matcher seam", t, func() {
		Convey("When the query uses only message-backed fields", func() {
			q, err := Parse("state=retry error~timeout retries>=2")
			So(err, ShouldBeNil)

			Convey("Then CompileMatcher yields a working env-free predicate", func() {
				m, cerr := CompileMatcher(q, "retry", now)
				So(cerr, ShouldBeNil)
				ti := testTask("q", "t", "a", asynq.TaskStateRetry, "{}")
				ti.Retried, ti.LastErr = 3, "gateway timeout"
				So(m(ti), ShouldBeTrue)
				ti.Retried = 1
				So(m(ti), ShouldBeFalse)
			})
		})

		Convey("When the query needs live worker/lease data", func() {
			q, err := Parse("state=active orphaned")
			So(err, ShouldBeNil)

			Convey("Then CompileMatcher refuses with a structured, positioned error", func() {
				_, cerr := CompileMatcher(q, "active", now)
				So(cerr, ShouldNotBeNil)
				So(cerr.Msg, ShouldContainSubstring, "`orphaned`")
				So(cerr.Hint, ShouldContainSubstring, "phase 12")
			})
		})
	})
}

func TestCursorEncoding(t *testing.T) {
	Convey("Given cursor wire encoding", t, func() {
		Convey("When a (queue, score, id) cursor round-trips", func() {
			c := Cursor{Queue: "email", Score: 1753444800, ID: "abc"}
			out, err := DecodeCursor(EncodeCursor(c))
			So(err, ShouldBeNil)
			So(out, ShouldResemble, c)
		})

		Convey("When a scan cursor round-trips", func() {
			c := ScanCursor{Queue: "email", Group: "g", Page: 7}
			out, err := DecodeScanCursor(EncodeScanCursor(c))
			So(err, ShouldBeNil)
			So(out, ShouldResemble, c)
		})

		Convey("When garbage is decoded", func() {
			_, err := DecodeCursor("not-a-cursor")
			So(err, ShouldNotBeNil)
		})
	})
}

func TestIsQuery(t *testing.T) {
	Convey("Given the AQL-vs-free-text sniffer", t, func() {
		Convey("Then clause-shaped strings are AQL", func() {
			So(IsQuery("queue=email"), ShouldBeTrue)
			So(IsQuery(`state=retry error~"gateway timeout"`), ShouldBeTrue)
			So(IsQuery("past_due"), ShouldBeTrue)
			So(IsQuery("orphaned"), ShouldBeTrue)
		})

		Convey("Then plain free text is not (legacy substring search keeps its semantics)", func() {
			So(IsQuery("gateway timeout"), ShouldBeFalse)
			So(IsQuery("email:send"), ShouldBeFalse)
			So(IsQuery(""), ShouldBeFalse)
		})
	})
}

func TestQueryHelpers(t *testing.T) {
	Convey("Given parsed queries", t, func() {
		Convey("When reading the queue filter", func() {
			q, err := Parse("queue=email state=retry")
			So(err, ShouldBeNil)
			So(q.QueueFilter(), ShouldEqual, "email")
		})

		Convey("When no queue clause exists", func() {
			q, err := Parse("state=retry")
			So(err, ShouldBeNil)
			So(q.QueueFilter(), ShouldEqual, "")
		})

		Convey("When listing allowed states for a multi-state query", func() {
			q, err := Parse("error~timeout")
			So(err, ShouldBeNil)
			So(strings.Join(q.AllowedStates(), ","), ShouldEqual, "retry,archived")
		})
	})
}
