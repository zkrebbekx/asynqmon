package asynqmon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Integration tests for GET /api/jobs/{id}/results — the completed scan
// job's own enumerated result set, paged and hydrated. Same harness as
// jobs_handlers_test.go: real Redis on 127.0.0.1:6379 DB 12, fixtures seeded
// through the real asynq client, previews run by the real claiming runner.
//
// This endpoint exists because re-running a query after a scan-to-completion
// job finishes as a FRESH budgeted scan drops every match beyond the first
// budget window — the job already enumerated the exact set; these tests pin
// that the persisted candidate list pages back exactly.
//
// Convey discipline: flows that mutate queues / run the runner execute
// imperatively BEFORE the Convey tree; the tree only reads captured results.
// ****************************************************************************

// resultRow decodes the frozen searchTask row shape (/api/tasks contract).
type resultRow struct {
	ID      string `json:"id"`
	Queue   string `json:"queue"`
	Type    string `json:"type"`
	Payload string `json:"payload"`
	State   string `json:"state"`
}

type resultsPage struct {
	Tasks           []resultRow `json:"tasks"`
	TotalCandidates int64       `json:"total_candidates"`
	Offset          int64       `json:"offset"`
	Vanished        int         `json:"vanished"`
}

// createScanJob creates a preview job over the scope and waits for the
// runner to finish enumeration (the preview_ready park).
func createScanJob(t *testing.T, env *jobsTestEnv, router http.Handler, scope map[string]interface{}) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
		"verb":   "archive",
		"scope":  scope,
		"reason": "scan-to-completion results test",
	}, &created)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating job: %d %s", w.Code, w.Body.String())
	}
	awaitJob(t, env.store, created.ID, "preview_ready",
		func(j *jobs.Job) bool { return j.State == jobs.StatePreviewReady })
	return created.ID
}

func getResults(t *testing.T, router http.Handler, jobID, params string) (resultsPage, int) {
	t.Helper()
	var page resultsPage
	w := doJSON(t, router, "GET", fmt.Sprintf("/api/jobs/%s/results?%s", jobID, params), nil, &page)
	return page, w.Code
}

// ----------------------------------------------------------------------------
// Paging exactness: the pages of /results are exactly the enumerated
// matches; a task deleted after the scan is skipped and honestly counted.
// ----------------------------------------------------------------------------

func TestJobResultsPagingExactness(t *testing.T) {
	env := newJobsTestEnv(t)
	env.startTestRunner(t)
	router := env.newRouter(Options{})

	// 12 matching (kind=a) + 5 decoys (kind=b) in one queue.
	seedPending(t, env, "res", "email:send", `{"kind":"a"}`, 12)
	seedPending(t, env, "res", "email:send", `{"kind":"b"}`, 5)
	jobID := createScanJob(t, env, router, map[string]interface{}{
		"queue": "res", "state": "pending", "meta": []string{"kind:a"},
	})

	// The seeded ground truth: every pending kind=a task id.
	wantIDs := map[string]bool{}
	pending, err := env.insp.ListPendingTasks("res", asynq.PageSize(100))
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	for _, ti := range pending {
		var p map[string]string
		if json.Unmarshal(ti.Payload, &p) == nil && p["kind"] == "a" {
			wantIDs[ti.ID] = true
		}
	}

	// Page through with limit=5: 5 + 5 + 2, then past the end.
	page0, code0 := getResults(t, router, jobID, "offset=0&limit=5")
	page1, code1 := getResults(t, router, jobID, "offset=5&limit=5")
	page2, code2 := getResults(t, router, jobID, "offset=10&limit=5")
	pageEnd, codeEnd := getResults(t, router, jobID, "offset=100&limit=5")
	gotIDs := map[string]bool{}
	dupes := 0
	for _, p := range []resultsPage{page0, page1, page2} {
		for _, row := range p.Tasks {
			if gotIDs[row.ID] {
				dupes++
			}
			gotIDs[row.ID] = true
		}
	}

	// The completed job carries the banner's "as of" stamp.
	var detail struct {
		PreviewCompletedAt string `json:"preview_completed_at"`
		FinishedAt         string `json:"finished_at"`
	}
	doJSON(t, router, "GET", "/api/jobs/"+jobID, nil, &detail)

	// Delete one enumerated match behind the job's back, then re-read its page.
	deletedID := page0.Tasks[0].ID
	if err := env.insp.DeleteTask("res", deletedID); err != nil {
		t.Fatalf("deleting task: %v", err)
	}
	pageAfter, codeAfter := getResults(t, router, jobID, "offset=0&limit=5")
	afterIDs := map[string]bool{}
	for _, row := range pageAfter.Tasks {
		afterIDs[row.ID] = true
	}

	Convey("Given a completed scan job over 12 matching and 5 decoy pending tasks", t, func() {
		Convey("When the results are paged through with limit=5", func() {
			Convey("Then the pages are exactly the 12 enumerated matches, no dupes, exact total on every page", func() {
				So(code0, ShouldEqual, http.StatusOK)
				So(code1, ShouldEqual, http.StatusOK)
				So(code2, ShouldEqual, http.StatusOK)
				So(page0.Tasks, ShouldHaveLength, 5)
				So(page1.Tasks, ShouldHaveLength, 5)
				So(page2.Tasks, ShouldHaveLength, 2)
				So(dupes, ShouldEqual, 0)
				So(gotIDs, ShouldHaveLength, 12)
				for id := range gotIDs {
					So(wantIDs[id], ShouldBeTrue)
				}
				So(page0.TotalCandidates, ShouldEqual, 12)
				So(page1.TotalCandidates, ShouldEqual, 12)
				So(page2.TotalCandidates, ShouldEqual, 12)
				So(page0.Offset, ShouldEqual, 0)
				So(page1.Offset, ShouldEqual, 5)
				So(page0.Vanished+page1.Vanished+page2.Vanished, ShouldEqual, 0)
			})

			Convey("Then rows are /api/tasks-shaped: id, queue, type, payload, state", func() {
				row := page0.Tasks[0]
				So(row.ID, ShouldNotBeEmpty)
				So(row.Queue, ShouldEqual, "res")
				So(row.Type, ShouldEqual, "email:send")
				So(row.Payload, ShouldContainSubstring, `"kind":"a"`)
				So(row.State, ShouldEqual, "pending")
			})

			Convey("Then a page past the end is empty, not an error", func() {
				So(codeEnd, ShouldEqual, http.StatusOK)
				So(pageEnd.Tasks, ShouldHaveLength, 0)
				So(pageEnd.Vanished, ShouldEqual, 0)
				So(pageEnd.TotalCandidates, ShouldEqual, 12)
			})
		})

		Convey("When the job detail is read after enumeration", func() {
			Convey("Then it stamps preview_completed_at even though the job is not terminal", func() {
				So(detail.PreviewCompletedAt, ShouldNotBeEmpty)
				So(detail.FinishedAt, ShouldBeEmpty) // preview_ready is a park, not a finish
			})
		})

		Convey("When one enumerated task was deleted after the scan", func() {
			Convey("Then its page skips it and states vanished=1 — never an error", func() {
				So(codeAfter, ShouldEqual, http.StatusOK)
				So(pageAfter.Vanished, ShouldEqual, 1)
				So(pageAfter.Tasks, ShouldHaveLength, 4)
				So(afterIDs[deletedID], ShouldBeFalse)
				So(pageAfter.TotalCandidates, ShouldEqual, 12) // refs persist; the skip is per-page
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Limit clamps + unknown-job 404.
// ----------------------------------------------------------------------------

func TestJobResultsLimitClampsAndNotFound(t *testing.T) {
	env := newJobsTestEnv(t)
	env.startTestRunner(t)
	router := env.newRouter(Options{})

	// 230 matches (whole queue matches the scope) so the clamps are visible.
	seedPending(t, env, "resclamp", "demo:bulk", `{"n":1}`, 230)
	jobID := createScanJob(t, env, router, map[string]interface{}{
		"queue": "resclamp", "state": "pending",
	})

	pageDefault, codeDefault := getResults(t, router, jobID, "")
	pageMax, codeMax := getResults(t, router, jobID, "limit=200")
	pageOver, codeOver := getResults(t, router, jobID, "limit=500")
	pageZero, codeZero := getResults(t, router, jobID, "limit=0")
	pageNegOff, codeNegOff := getResults(t, router, jobID, "offset=-9&limit=5")
	pageTail, codeTail := getResults(t, router, jobID, "offset=225&limit=7")
	_, codeMissing := getResults(t, router, "jb_nope", "limit=5")

	Convey("Given a completed scan job with 230 enumerated matches", t, func() {
		Convey("When pages are requested with assorted limits", func() {
			Convey("Then no limit means the default 50", func() {
				So(codeDefault, ShouldEqual, http.StatusOK)
				So(pageDefault.Tasks, ShouldHaveLength, 50)
				So(pageDefault.TotalCandidates, ShouldEqual, 230)
			})
			Convey("Then the 200 ceiling is served in full", func() {
				So(codeMax, ShouldEqual, http.StatusOK)
				So(pageMax.Tasks, ShouldHaveLength, 200)
			})
			Convey("Then out-of-range limits fall back to the default 50", func() {
				So(codeOver, ShouldEqual, http.StatusOK)
				So(pageOver.Tasks, ShouldHaveLength, 50)
				So(codeZero, ShouldEqual, http.StatusOK)
				So(pageZero.Tasks, ShouldHaveLength, 50)
			})
			Convey("Then a negative offset reads from the start", func() {
				So(codeNegOff, ShouldEqual, http.StatusOK)
				So(pageNegOff.Offset, ShouldEqual, 0)
				So(pageNegOff.Tasks, ShouldHaveLength, 5)
			})
			Convey("Then the tail page returns the remainder", func() {
				So(codeTail, ShouldEqual, http.StatusOK)
				So(pageTail.Tasks, ShouldHaveLength, 5)
			})
		})

		Convey("When an unknown job's results are requested", func() {
			Convey("Then it 404s", func() {
				So(codeMissing, ShouldEqual, http.StatusNotFound)
			})
		})
	})
}
