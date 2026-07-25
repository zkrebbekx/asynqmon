package asynqmon

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Phase-12 "AQL env-at-scale" integration test on DB 12 (the jobs suite's
// database; helpers shared with jobs_handlers_test.go): an env-dependent job
// scope (pending_age>) enumerates AND executes correctly through the
// aql.EnvBuilder session — the predicate that phase 6 rejected at job-create
// time.
//
// Fixture note (documented hand-built-key exception, the stats suite's
// precedent): tasks are enqueued through the real asynq client, then three
// tasks' pending_since hash fields are backdated directly — aging a pending
// task authentically would mean sleeping for the threshold, and a sub-second
// threshold would race the execute phase (by act time every task would
// qualify). The field's format (Unix nanoseconds on the task hash) is pinned
// by TestMirroredKeyFormats in the stats suite.
// ****************************************************************************

func TestJobEnvScopePendingAge(t *testing.T) {
	env := newJobsTestEnv(t)
	ctx := context.Background()

	// 5 authentic pending tasks; backdate 3 of them by one hour.
	var old, young []string
	for i := 0; i < 5; i++ {
		ti, err := env.client.Enqueue(asynq.NewTask("env:job", []byte(`{"n":1}`)), asynq.Queue("envq"))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if i < 3 {
			old = append(old, ti.ID)
		} else {
			young = append(young, ti.ID)
		}
	}
	backdated := time.Now().Add(-time.Hour).UnixNano()
	for _, id := range old {
		if err := env.rc.HSet(ctx, "asynq:{envq}:t:"+id, "pending_since",
			strconv.FormatInt(backdated, 10)).Err(); err != nil {
			t.Fatalf("backdating pending_since: %v", err)
		}
	}

	h := env.newRouter(Options{})
	env.startTestRunner(t)

	// Create the env-scoped job: archive every pending task older than 30m.
	var created struct {
		ID string `json:"id"`
	}
	createW := doJSON(t, h, "POST", "/api/jobs", map[string]interface{}{
		"verb": "archive",
		"scope": map[string]interface{}{
			"queue": "envq", "state": "pending", "q": "", "meta": []string{},
			"aql": "pending_age>30m",
		},
		"reason": "phase-12 env scope test",
	}, &created)
	if createW.Code != 201 {
		t.Fatalf("create: status=%d body=%s", createW.Code, createW.Body.String())
	}

	previewed := awaitJob(t, env.store, created.ID, "preview complete", func(j *jobs.Job) bool {
		return j.PreviewComplete
	})

	// Execute and wait for completion.
	doJSON(t, h, "POST", "/api/jobs/"+created.ID+"/execute", map[string]interface{}{}, nil)
	done := awaitJob(t, env.store, created.ID, "job done", func(j *jobs.Job) bool {
		return j.State == jobs.StateDone
	})

	qi, qerr := env.insp.GetQueueInfo("envq")

	Convey("Given a job scoped by the env-dependent pending_age predicate", t, func() {
		Convey("Then the preview enumerates exactly the aged tasks (env prepared per batch)", func() {
			So(previewed.Counts.Candidates, ShouldEqual, 3)
		})
		Convey("Then execution re-verifies with live env data and acts on all three", func() {
			So(done.Counts.Acted, ShouldEqual, 3)
			So(done.Counts.Failed, ShouldEqual, 0)
			So(done.Counts.Skipped, ShouldEqual, 0)
		})
		Convey("Then the queue reflects it: 3 archived, 2 still pending (too young)", func() {
			So(qerr, ShouldBeNil)
			So(qi.Archived, ShouldEqual, 3)
			So(qi.Pending, ShouldEqual, 2)
		})
		Convey("And the archived set is exactly the backdated tasks", func() {
			archived, err := env.insp.ListArchivedTasks("envq")
			So(err, ShouldBeNil)
			ids := map[string]bool{}
			for _, ti := range archived {
				ids[ti.ID] = true
			}
			for _, id := range old {
				So(ids[id], ShouldBeTrue)
			}
			for _, id := range young {
				So(ids[id], ShouldBeFalse)
			}
		})
	})
}
