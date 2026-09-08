package asynqmon

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests for a producer-controlled name that needs an encoded
// path segment. asynq puts no restriction on a queue name, so a producer may
// create "tenant/acme". Before this suite the console could list such a
// queue but never address it: every API route carries the queue as ONE path
// segment, and mux matched the DECODED path, where "%2F" is a real "/".
//
// The router now matches on the raw path (mux.Router.UseEncodedPath) and
// decodePathVars unescapes each variable once before the handler runs. These
// tests drive the real router (muxRouter) against a real Redis on DB 12 —
// the jobs suite's database, flushed by newJobsTestEnv.
//
// Convey discipline: the Redis work runs imperatively BEFORE the Convey
// tree; the tree only reads captured results (jobs_handlers_test.go
// pattern).
// ****************************************************************************

// slashQueue is the name under test. seg("tenant/acme") is the encoded form
// the console sends.
const (
	slashQueue    = "tenant/acme"
	slashQueueEnc = "tenant%2Facme"
	// punctQueue exercises the names that already worked, so the raw-path
	// switch does not regress them.
	punctQueue    = "a#b?c%d"
	punctQueueEnc = "a%23b%3Fc%25d"
)

// do sends one request through the real router and returns the recorder.
// It uses httptest.NewRequest, which parses the target through the HTTP
// request-line parser, so the raw path (and its "%2F") survives.
func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ----------------------------------------------------------------------------
// Read paths.
// ----------------------------------------------------------------------------

func TestSlashQueueNameReadPaths(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{})

	pending, err := env.client.Enqueue(asynq.NewTask("slash:read", []byte(`{}`)), asynq.Queue(slashQueue))
	if err != nil {
		t.Fatalf("seeding pending task: %v", err)
	}
	if _, err := env.client.Enqueue(
		asynq.NewTask("slash:grouped", []byte(`{}`)),
		asynq.Queue(slashQueue), asynq.Group("team/billing"),
	); err != nil {
		t.Fatalf("seeding aggregating task: %v", err)
	}
	if _, err := env.client.Enqueue(asynq.NewTask("punct:read", []byte(`{}`)), asynq.Queue(punctQueue)); err != nil {
		t.Fatalf("seeding punctuation queue: %v", err)
	}

	var queueResp struct {
		Current struct {
			Queue string `json:"queue"`
		} `json:"current"`
	}
	wQueue := doJSON(t, h, "GET", "/api/queues/"+slashQueueEnc, nil, &queueResp)

	var listResp struct {
		Tasks []struct {
			ID    string `json:"id"`
			Queue string `json:"queue"`
		} `json:"tasks"`
	}
	wList := doJSON(t, h, "GET", "/api/queues/"+slashQueueEnc+"/pending_tasks", nil, &listResp)

	var detailResp struct {
		ID    string `json:"id"`
		Queue string `json:"queue"`
	}
	wDetail := doJSON(t, h, "GET", "/api/queues/"+slashQueueEnc+"/tasks/"+url.PathEscape(pending.ID), nil, &detailResp)

	var aggResp struct {
		Tasks []struct {
			Queue string `json:"queue"`
		} `json:"tasks"`
	}
	wAgg := doJSON(t, h,
		"GET", "/api/queues/"+slashQueueEnc+"/groups/team%2Fbilling/aggregating_tasks", nil, &aggResp)

	var punctResp struct {
		Current struct {
			Queue string `json:"queue"`
		} `json:"current"`
	}
	wPunct := doJSON(t, h, "GET", "/api/queues/"+punctQueueEnc, nil, &punctResp)

	// A bare "/" is still two segments and must NOT reach the queue handler.
	wBare := do(t, h, "GET", "/api/queues/tenant/acme")

	// A mounted sub-path still matches: PathPrefix now compares against the
	// raw path too, and a root path holds no character that needs escaping.
	hRooted := env.newRouter(Options{RootPath: "/monitoring"})
	var rootedResp struct {
		Current struct {
			Queue string `json:"queue"`
		} `json:"current"`
	}
	wRooted := doJSON(t, hRooted, "GET", "/monitoring/api/queues/"+slashQueueEnc, nil, &rootedResp)

	Convey("Given a queue whose name contains a slash", t, func() {
		Convey("When the console reads it as one encoded path segment", func() {
			Convey("Then the handler answers 200 for the decoded name", func() {
				So(wQueue.Code, ShouldEqual, http.StatusOK)
				So(queueResp.Current.Queue, ShouldEqual, slashQueue)
			})
			Convey("Then the task list answers 200 and reports the same name", func() {
				So(wList.Code, ShouldEqual, http.StatusOK)
				So(len(listResp.Tasks), ShouldEqual, 1)
				So(listResp.Tasks[0].Queue, ShouldEqual, slashQueue)
			})
			Convey("Then the task detail resolves the task in that queue", func() {
				So(wDetail.Code, ShouldEqual, http.StatusOK)
				So(detailResp.ID, ShouldEqual, pending.ID)
				So(detailResp.Queue, ShouldEqual, slashQueue)
			})
		})
		Convey("When the group name also contains a slash", func() {
			Convey("Then the aggregating list answers 200 for that group", func() {
				So(wAgg.Code, ShouldEqual, http.StatusOK)
				So(len(aggResp.Tasks), ShouldEqual, 1)
				So(aggResp.Tasks[0].Queue, ShouldEqual, slashQueue)
			})
		})
		Convey("When the client sends a bare slash instead of %2F", func() {
			Convey("Then no queue route matches and the SPA fallback answers", func() {
				So(wBare.Code, ShouldNotEqual, http.StatusOK)
			})
		})
		Convey("When the handler is mounted under a root path", func() {
			Convey("Then the same encoded name still reaches the handler", func() {
				So(wRooted.Code, ShouldEqual, http.StatusOK)
				So(rootedResp.Current.Queue, ShouldEqual, slashQueue)
			})
		})
	})

	Convey("Given a queue named with '#', '?' and '%'", t, func() {
		Convey("When the console reads it", func() {
			Convey("Then it still answers 200 for the decoded name", func() {
				So(wPunct.Code, ShouldEqual, http.StatusOK)
				So(punctResp.Current.Queue, ShouldEqual, punctQueue)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Mutate paths.
// ----------------------------------------------------------------------------

func TestSlashQueueNameMutatePaths(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{})

	first, err := env.client.Enqueue(asynq.NewTask("slash:mutate", []byte(`{}`)), asynq.Queue(slashQueue))
	if err != nil {
		t.Fatalf("seeding first task: %v", err)
	}
	second, err := env.client.Enqueue(asynq.NewTask("slash:mutate", []byte(`{"n":2}`)), asynq.Queue(slashQueue))
	if err != nil {
		t.Fatalf("seeding second task: %v", err)
	}

	base := "/api/queues/" + slashQueueEnc

	wPause := do(t, h, "POST", base+":pause")
	pausedInfo, pausedErr := env.insp.GetQueueInfo(slashQueue)
	wResume := do(t, h, "POST", base+":resume")
	resumedInfo, resumedErr := env.insp.GetQueueInfo(slashQueue)

	wArchive := do(t, h, "POST", base+"/pending_tasks/"+url.PathEscape(first.ID)+":archive")
	archived, archivedErr := env.insp.ListArchivedTasks(slashQueue)

	wRun := do(t, h, "POST", base+"/archived_tasks/"+url.PathEscape(first.ID)+":run")
	afterRun, afterRunErr := env.insp.GetQueueInfo(slashQueue)

	wBatch := doJSON(t, h, "POST", base+"/pending_tasks:batch_delete",
		map[string]interface{}{"task_ids": []string{second.ID}}, nil)
	afterBatch, afterBatchErr := env.insp.GetQueueInfo(slashQueue)

	// A punctuation-only name keeps working on a mutate route too.
	if _, err := env.client.Enqueue(asynq.NewTask("punct:mutate", nil), asynq.Queue(punctQueue)); err != nil {
		t.Fatalf("seeding punctuation queue: %v", err)
	}
	wPunctPause := do(t, h, "POST", "/api/queues/"+punctQueueEnc+":pause")
	punctInfo, punctErr := env.insp.GetQueueInfo(punctQueue)

	Convey("Given a queue whose name contains a slash", t, func() {
		Convey("When the console pauses and resumes it", func() {
			Convey("Then both answer 204 and the queue state follows", func() {
				So(wPause.Code, ShouldEqual, http.StatusNoContent)
				So(pausedErr, ShouldBeNil)
				So(pausedInfo.Paused, ShouldBeTrue)
				So(wResume.Code, ShouldEqual, http.StatusNoContent)
				So(resumedErr, ShouldBeNil)
				So(resumedInfo.Paused, ShouldBeFalse)
			})
		})
		Convey("When the console archives one pending task", func() {
			Convey("Then it answers 204 and that task is archived", func() {
				So(wArchive.Code, ShouldEqual, http.StatusNoContent)
				So(archivedErr, ShouldBeNil)
				So(len(archived), ShouldEqual, 1)
				So(archived[0].ID, ShouldEqual, first.ID)
			})
		})
		Convey("When the console runs the archived task back", func() {
			Convey("Then it answers 204 and nothing stays archived", func() {
				So(wRun.Code, ShouldEqual, http.StatusNoContent)
				So(afterRunErr, ShouldBeNil)
				So(afterRun.Archived, ShouldEqual, 0)
				So(afterRun.Pending, ShouldEqual, 2)
			})
		})
		Convey("When the console batch-deletes a task by id", func() {
			Convey("Then it answers 200 and the queue loses that task", func() {
				So(wBatch.Code, ShouldEqual, http.StatusOK)
				So(afterBatchErr, ShouldBeNil)
				So(afterBatch.Pending, ShouldEqual, 1)
			})
		})
	})

	Convey("Given a queue named with '#', '?' and '%'", t, func() {
		Convey("When the console pauses it", func() {
			Convey("Then it answers 204 and the queue is paused", func() {
				So(wPunctPause.Code, ShouldEqual, http.StatusNoContent)
				So(punctErr, ShouldBeNil)
				So(punctInfo.Paused, ShouldBeTrue)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Delete queue: the one route that removes the queue itself.
// ----------------------------------------------------------------------------

func TestSlashQueueNameDeleteQueue(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{})

	if _, err := env.client.Enqueue(asynq.NewTask("slash:gone", nil), asynq.Queue(slashQueue)); err != nil {
		t.Fatalf("seeding queue: %v", err)
	}
	// DeleteQueue refuses a non-empty queue, so drain it first.
	if _, err := env.insp.DeleteAllPendingTasks(slashQueue); err != nil {
		t.Fatalf("draining queue: %v", err)
	}

	w := do(t, h, "DELETE", "/api/queues/"+slashQueueEnc)
	names, listErr := env.insp.Queues()

	Convey("Given an empty queue whose name contains a slash", t, func() {
		Convey("When the console deletes it", func() {
			Convey("Then it answers 204 and the queue is gone", func() {
				So(w.Code, ShouldEqual, http.StatusNoContent)
				So(listErr, ShouldBeNil)
				So(names, ShouldNotContain, slashQueue)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Read-only mode still wins over the decode middleware.
// ----------------------------------------------------------------------------

func TestSlashQueueNameReadOnlyStillRejectsMutations(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{ReadOnly: true})

	if _, err := env.client.Enqueue(asynq.NewTask("slash:ro", nil), asynq.Queue(slashQueue)); err != nil {
		t.Fatalf("seeding queue: %v", err)
	}

	wGet := do(t, h, "GET", "/api/queues/"+slashQueueEnc)
	wPause := do(t, h, "POST", "/api/queues/"+slashQueueEnc+":pause")

	Convey("Given a read-only server and a queue named with a slash", t, func() {
		Convey("When the console reads and then tries to pause it", func() {
			Convey("Then the read answers 200 and the mutation answers the read-only 405", func() {
				So(wGet.Code, ShouldEqual, http.StatusOK)
				So(wPause.Code, ShouldEqual, http.StatusMethodNotAllowed)
				So(wPause.Body.String(), ShouldContainSubstring, "read-only mode")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Every other producer-controlled path variable decodes the same way.
// ----------------------------------------------------------------------------

func TestSlashTaskIDDecodes(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{})

	// asynq.TaskID accepts any non-empty id, so a producer may choose one
	// with a slash in it.
	const taskID = "batch/2026-09-08"
	if _, err := env.client.Enqueue(
		asynq.NewTask("slash:id", nil), asynq.Queue(slashQueue), asynq.TaskID(taskID),
	); err != nil {
		t.Fatalf("seeding task with a slash in the id: %v", err)
	}

	var detail struct {
		ID    string `json:"id"`
		Queue string `json:"queue"`
	}
	w := doJSON(t, h, "GET",
		"/api/queues/"+slashQueueEnc+"/tasks/"+url.PathEscape(taskID), nil, &detail)

	Convey("Given a task whose id contains a slash", t, func() {
		Convey("When the console reads the task detail", func() {
			Convey("Then the handler resolves the decoded id", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(detail.ID, ShouldEqual, taskID)
				So(detail.Queue, ShouldEqual, slashQueue)
			})
		})
	})
}
