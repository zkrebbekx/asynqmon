package jobs

import (
	"testing"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Pure-logic tests for the Scope predicate, the verb capability matrix, and
// the cost classifier. No Redis: the integration suite (root package
// jobs_handlers_test.go, DB 12) covers persistence, fencing, and the runner.
// ****************************************************************************

func task(queue, typ, id string, state asynq.TaskState, payload string) *asynq.TaskInfo {
	return &asynq.TaskInfo{
		ID:      id,
		Queue:   queue,
		Type:    typ,
		State:   state,
		Payload: []byte(payload),
	}
}

func TestScopeMatches(t *testing.T) {
	Convey("Given a scope over queue=email state=retry with free text and meta filters", t, func() {
		scope := Scope{
			Queue: "email",
			State: "retry",
			Q:     "gateway",
			Meta:  []string{"region:eu", "attempt:3"},
		}

		Convey("When a task matches queue, state, text, and every meta pair", func() {
			ti := task("email", "email:send", "id-1", asynq.TaskStateRetry,
				`{"region":"eu","attempt":3,"err":"gateway timeout"}`)

			Convey("Then it matches", func() {
				So(scope.Matches(ti), ShouldBeTrue)
			})
		})

		Convey("When the task is in another queue", func() {
			ti := task("billing", "email:send", "id-1", asynq.TaskStateRetry,
				`{"region":"eu","attempt":3,"err":"gateway timeout"}`)

			Convey("Then it does not match", func() {
				So(scope.Matches(ti), ShouldBeFalse)
			})
		})

		Convey("When the task changed state since enumeration (the TOCTOU case)", func() {
			ti := task("email", "email:send", "id-1", asynq.TaskStateArchived,
				`{"region":"eu","attempt":3,"err":"gateway timeout"}`)

			Convey("Then re-matching rejects it so execution counts it as skipped", func() {
				So(scope.Matches(ti), ShouldBeFalse)
			})
		})

		Convey("When a meta value differs", func() {
			ti := task("email", "email:send", "id-1", asynq.TaskStateRetry,
				`{"region":"us","attempt":3,"err":"gateway timeout"}`)

			Convey("Then it does not match", func() {
				So(scope.Matches(ti), ShouldBeFalse)
			})
		})

		Convey("When the free text appears nowhere in id/type/queue/payload", func() {
			ti := task("email", "email:send", "id-1", asynq.TaskStateRetry,
				`{"region":"eu","attempt":3,"err":"connection refused"}`)

			Convey("Then it does not match", func() {
				So(scope.Matches(ti), ShouldBeFalse)
			})
		})

		Convey("When the task is nil", func() {
			Convey("Then it does not match (and does not panic)", func() {
				So(scope.Matches(nil), ShouldBeFalse)
			})
		})
	})

	Convey("Given a whole-fleet scope (queue=all, no text, no meta)", t, func() {
		scope := Scope{Queue: "all", State: "pending"}

		Convey("When any pending task is tested", func() {
			ti := task("anything", "t", "id", asynq.TaskStatePending, `not-json`)

			Convey("Then it matches — non-JSON payloads only fail meta filters", func() {
				So(scope.Matches(ti), ShouldBeTrue)
			})
		})

		Convey("When a meta filter is added and the payload is not JSON", func() {
			withMeta := Scope{Queue: "all", State: "pending", Meta: []string{"k:v"}}
			ti := task("anything", "t", "id", asynq.TaskStatePending, `not-json`)

			Convey("Then it does not match", func() {
				So(withMeta.Matches(ti), ShouldBeFalse)
			})
		})
	})

	Convey("Given the free-text match is case-insensitive over the type", t, func() {
		scope := Scope{Queue: "all", State: "pending", Q: "EMAIL:SEND"}

		Convey("When a lower-case-typed task is tested", func() {
			ti := task("q", "email:send", "id", asynq.TaskStatePending, `{}`)

			Convey("Then it matches", func() {
				So(scope.Matches(ti), ShouldBeTrue)
			})
		})
	})
}

func TestValidateScopeVerb(t *testing.T) {
	Convey("Given the per-state verb capability matrix", t, func() {
		Convey("When supported combinations are validated", func() {
			cases := []struct {
				state string
				verb  Verb
			}{
				{"active", VerbCancel},
				{"pending", VerbArchive},
				{"pending", VerbDelete},
				{"scheduled", VerbRun},
				{"retry", VerbArchive},
				{"archived", VerbRun},
				{"completed", VerbDelete},
				{"aggregating", VerbRun},
			}
			Convey("Then every one passes", func() {
				for _, c := range cases {
					So(ValidateScopeVerb(Scope{State: c.state}, c.verb), ShouldBeEmpty)
				}
			})
		})

		Convey("When unsupported combinations are validated", func() {
			cases := []struct {
				state string
				verb  Verb
			}{
				{"active", VerbDelete},   // active tasks can only be canceled
				{"pending", VerbRun},     // pending is already runnable
				{"archived", VerbCancel}, // nothing to cancel
				{"completed", VerbRun},
			}
			Convey("Then every one is rejected with a reason", func() {
				for _, c := range cases {
					So(ValidateScopeVerb(Scope{State: c.state}, c.verb), ShouldNotBeEmpty)
				}
			})
		})

		Convey("When the state itself is invalid", func() {
			Convey("Then the reason names the state", func() {
				So(ValidateScopeVerb(Scope{State: "bogus"}, VerbRun), ShouldContainSubstring, "bogus")
			})
		})
	})
}

func TestComputeCostClass(t *testing.T) {
	Convey("Given the §4.3 cost-class rule", t, func() {
		Convey("When delete or archive targets a LIST state (pending/active)", func() {
			Convey("Then the class is list_removal", func() {
				So(ComputeCostClass(Scope{State: "pending"}, VerbDelete), ShouldEqual, CostListRemoval)
				So(ComputeCostClass(Scope{State: "pending"}, VerbArchive), ShouldEqual, CostListRemoval)
				So(ComputeCostClass(Scope{State: "active"}, VerbDelete), ShouldEqual, CostListRemoval)
			})
		})

		Convey("When the state is ZSET-backed or the verb only reads/enqueues", func() {
			Convey("Then the class is cheap", func() {
				So(ComputeCostClass(Scope{State: "retry"}, VerbDelete), ShouldEqual, CostCheap)
				So(ComputeCostClass(Scope{State: "archived"}, VerbRun), ShouldEqual, CostCheap)
				So(ComputeCostClass(Scope{State: "active"}, VerbCancel), ShouldEqual, CostCheap)
				So(ComputeCostClass(Scope{State: "scheduled"}, VerbArchive), ShouldEqual, CostCheap)
			})
		})
	})
}

func TestStateIsTerminal(t *testing.T) {
	Convey("Given the job lifecycle states", t, func() {
		Convey("When terminality is checked", func() {
			Convey("Then done/canceled/failed are terminal and the rest are not", func() {
				So(StateDone.IsTerminal(), ShouldBeTrue)
				So(StateCanceled.IsTerminal(), ShouldBeTrue)
				So(StateFailed.IsTerminal(), ShouldBeTrue)
				So(StatePreviewing.IsTerminal(), ShouldBeFalse)
				So(StatePreviewReady.IsTerminal(), ShouldBeFalse)
				So(StateRunning.IsTerminal(), ShouldBeFalse)
				So(StatePaused.IsTerminal(), ShouldBeFalse)
			})
		})
	})
}
