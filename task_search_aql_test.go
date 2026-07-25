package asynqmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Integration tests for the phase-6 AQL surface of GET /api/tasks and the
// new GET /api/tasks/state_counts — against a real Redis on 127.0.0.1:6379
// DB 12 (shared with jobs_handlers_test.go; tests in this package run
// sequentially and each env creation flushes the DB). Fixtures are seeded
// through the real asynq client — scheduled via ProcessIn, retry via a
// short-lived asynq server whose handler always fails — so key layouts and
// zset scores are authentic.
//
// Convey discipline: flows that touch Redis run imperatively BEFORE the
// Convey tree; the tree only reads captured results (jobs_handlers_test.go
// pattern).
// ****************************************************************************

// getJSON performs a GET and decodes the body regardless of status.
func aqlGetJSON(t *testing.T, h http.Handler, rawURL string, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", rawURL, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding GET %s response (status %d): %v\nbody: %s", rawURL, w.Code, err, w.Body.String())
		}
	}
	return w
}

func tasksURL(params map[string]string) string {
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	return "/api/tasks?" + v.Encode()
}

// aqlSearchResp mirrors searchTasksResponse's wire shape.
type aqlSearchResp struct {
	Tasks []struct {
		ID    string `json:"id"`
		Queue string `json:"queue"`
		Type  string `json:"type"`
		State string `json:"state"`
	} `json:"tasks"`
	Total             int64  `json:"total"`
	Scanned           int    `json:"scanned"`
	Truncated         bool   `json:"truncated"`
	Mode              string `json:"mode"`
	Exact             bool   `json:"exact"`
	State             string `json:"state"`
	Cursor            string `json:"cursor"`
	ScanCursor        string `json:"scan_cursor"`
	CandidateEstimate int64  `json:"candidate_estimate"`
	Budget            int    `json:"budget"`
}

// aqlErrorResp is the structured 400 rejection: {error, position, hint}.
type aqlErrorResp struct {
	Error    string `json:"error"`
	Position int    `json:"position"`
	Hint     string `json:"hint"`
}

func TestAqlCursorListingExactness(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// Seed: two queues × 12 scheduled tasks at 1..12 minutes out (inside the
	// 30m bound) + 3 at 2h out (outside). Exact expected total: 24.
	for _, qname := range []string{"aqla", "aqlb"} {
		for i := 1; i <= 12; i++ {
			if _, err := env.client.Enqueue(
				asynq.NewTask("job:sched", []byte(fmt.Sprintf(`{"n":%d}`, i))),
				asynq.Queue(qname), asynq.ProcessIn(time.Duration(i)*time.Minute),
			); err != nil {
				t.Fatalf("seeding scheduled: %v", err)
			}
		}
		for i := 0; i < 3; i++ {
			if _, err := env.client.Enqueue(
				asynq.NewTask("job:late", nil),
				asynq.Queue(qname), asynq.ProcessIn(2*time.Hour),
			); err != nil {
				t.Fatalf("seeding late scheduled: %v", err)
			}
		}
	}

	// Walk the whole result set in cursor pages of 5.
	var pages []aqlSearchResp
	seen := map[string]bool{}
	cursor := ""
	orderOK := true
	prevQueue, prevScoreQueue := "", ""
	_ = prevScoreQueue
	var prevID string
	_ = prevID
	for i := 0; i < 20; i++ { // hard stop: 20 pages is > 24/5
		params := map[string]string{"q": "state=scheduled next_run<30m", "size": "5"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var resp aqlSearchResp
		w := aqlGetJSON(t, h, tasksURL(params), &resp)
		if w.Code != 200 {
			t.Fatalf("cursor page %d: status %d body %s", i, w.Code, w.Body.String())
		}
		pages = append(pages, resp)
		for _, task := range resp.Tasks {
			key := task.Queue + "/" + task.ID
			if seen[key] {
				t.Fatalf("cursor pagination returned %s twice", key)
			}
			seen[key] = true
			// Queues must appear in sorted order, never interleaved back.
			if task.Queue < prevQueue {
				orderOK = false
			}
			prevQueue = task.Queue
		}
		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}

	Convey("Given 24 in-bound and 6 out-of-bound scheduled tasks across two queues", t, func() {
		Convey("When listing state=scheduled next_run<30m through cursor pages", func() {
			Convey("Then the plan is cursor-mode and exact", func() {
				So(pages[0].Mode, ShouldEqual, "cursor")
				So(pages[0].Exact, ShouldBeTrue)
				So(pages[0].State, ShouldEqual, "scheduled")
			})
			Convey("Then the total is the exact ZCOUNT sum", func() {
				So(pages[0].Total, ShouldEqual, 24)
			})
			Convey("Then walking the cursor yields every task exactly once", func() {
				So(len(seen), ShouldEqual, 24)
			})
			Convey("Then queues are traversed in sorted order", func() {
				So(orderOK, ShouldBeTrue)
			})
			Convey("Then the final page carries no cursor", func() {
				So(pages[len(pages)-1].Cursor, ShouldEqual, "")
			})
		})
	})
}

func TestAqlCursorListingRetryState(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// Seed retry through the real machinery: a server whose handler always
	// fails moves pending tasks into retry with next_process_at = now +
	// RetryDelayFunc — the authentic zset layout the cursor lists.
	const nRetry = 6
	for i := 0; i < nRetry; i++ {
		if _, err := env.client.Enqueue(
			asynq.NewTask("job:fails", []byte(`{}`)), asynq.Queue("aqlretry"),
		); err != nil {
			t.Fatalf("seeding pending: %v", err)
		}
	}
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB},
		asynq.Config{
			Concurrency: 4,
			Queues:      map[string]int{"aqlretry": 1},
			RetryDelayFunc: func(n int, e error, t *asynq.Task) time.Duration {
				return 90 * time.Minute
			},
			LogLevel: asynq.FatalLevel,
		},
	)
	mux := asynq.NewServeMux()
	mux.HandleFunc("job:fails", func(ctx context.Context, task *asynq.Task) error {
		return fmt.Errorf("boom: gateway timeout")
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting failing server: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		qi, err := env.insp.GetQueueInfo("aqlretry")
		if err == nil && qi.Retry == nRetry {
			break
		}
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("tasks never reached retry state (info: %+v, err: %v)", qi, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	srv.Shutdown()

	var within aqlSearchResp
	aqlGetJSON(t, h, tasksURL(map[string]string{"q": "state=retry next_run<2h", "size": "10"}), &within)
	var beyond aqlSearchResp
	aqlGetJSON(t, h, tasksURL(map[string]string{"q": "state=retry next_run<10m", "size": "10"}), &beyond)

	Convey("Given tasks driven into retry by a failing worker (next run ≈ now+90m)", t, func() {
		Convey("When querying next_run<2h", func() {
			Convey("Then the cursor listing is exact and complete", func() {
				So(within.Mode, ShouldEqual, "cursor")
				So(within.Exact, ShouldBeTrue)
				So(within.Total, ShouldEqual, nRetry)
				So(len(within.Tasks), ShouldEqual, nRetry)
			})
		})
		Convey("When querying next_run<10m", func() {
			Convey("Then the exact answer is zero — the storm has not landed", func() {
				So(beyond.Total, ShouldEqual, 0)
				So(len(beyond.Tasks), ShouldEqual, 0)
			})
		})
	})
}

func TestAqlProgressiveScanBudgetAndResume(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// 3 queues × 120 pending tasks; every even task is region=eu (60/queue).
	for _, qname := range []string{"scanq1", "scanq2", "scanq3"} {
		for i := 0; i < 120; i++ {
			region := "us"
			if i%2 == 0 {
				region = "eu"
			}
			if _, err := env.client.Enqueue(
				asynq.NewTask("job:scan", []byte(fmt.Sprintf(`{"region":%q,"n":%d}`, region, i))),
				asynq.Queue(qname),
			); err != nil {
				t.Fatalf("seeding pending: %v", err)
			}
		}
	}

	// Call 1: budget 150. The scan lists in 1000-task pages, so it consumes
	// whole queues until the budget is spent, then parks a cursor. The
	// first (cursor-less) call paginates its window legacy-style: Total is
	// every match in the scanned window, Tasks is the requested page of it.
	var first aqlSearchResp
	w1 := aqlGetJSON(t, h, tasksURL(map[string]string{
		"q": "state=pending meta.region=eu", "max_scan": "150", "size": "20",
	}), &first)

	// Follow the cursor to completion. Continuation calls return their
	// window's matches wholesale (the console appends them).
	unique := map[string]bool{}
	for _, task := range first.Tasks {
		unique[task.Queue+"/"+task.ID] = true
	}
	totalMatchesReported := first.Total
	totalScanned := first.Scanned
	cursor := first.ScanCursor
	calls := 1
	var last aqlSearchResp
	dupAcrossCalls := false
	for cursor != "" && calls < 10 {
		var resp aqlSearchResp
		aqlGetJSON(t, h, tasksURL(map[string]string{
			"q": "state=pending meta.region=eu", "max_scan": "150", "scan_cursor": cursor,
		}), &resp)
		for _, task := range resp.Tasks {
			key := task.Queue + "/" + task.ID
			if unique[key] {
				dupAcrossCalls = true
			}
			unique[key] = true
		}
		totalMatchesReported += resp.Total
		totalScanned += resp.Scanned
		cursor = resp.ScanCursor
		last = resp
		calls++
	}

	Convey("Given 360 pending tasks (180 matching) and a 150-task scan budget", t, func() {
		Convey("When the first call runs", func() {
			Convey("Then it is scan-mode with the honest §5.9 metering", func() {
				So(w1.Code, ShouldEqual, 200)
				So(first.Mode, ShouldEqual, "scan")
				So(first.Exact, ShouldBeFalse)
				So(first.Budget, ShouldEqual, 150)
				So(first.CandidateEstimate, ShouldEqual, 360)
			})
			Convey("Then it stops after the budgeted work with a resume cursor", func() {
				So(first.ScanCursor, ShouldNotEqual, "")
				So(first.Truncated, ShouldBeTrue) // back-compat flag still populated
				So(first.Scanned, ShouldBeLessThan, 360)
			})
			Convey("Then its window pagination is legacy-compatible", func() {
				So(len(first.Tasks), ShouldEqual, 20) // requested page size
				So(first.Total, ShouldBeGreaterThan, int64(len(first.Tasks)))
			})
		})
		Convey("When the cursor is followed to completion", func() {
			Convey("Then the windows' match totals sum to exactly the 180 region=eu tasks", func() {
				So(totalMatchesReported, ShouldEqual, 180)
			})
			Convey("Then no task appears in two windows", func() {
				So(dupAcrossCalls, ShouldBeFalse)
			})
			Convey("Then every task was scanned exactly once across calls", func() {
				So(totalScanned, ShouldEqual, 360)
			})
			Convey("Then the final call carries no cursor and is not truncated", func() {
				So(last.ScanCursor, ShouldEqual, "")
				So(last.Truncated, ShouldBeFalse)
			})
		})
	})
}

func TestAqlParseRejection400(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	var enq aqlErrorResp
	wEnq := aqlGetJSON(t, h, tasksURL(map[string]string{"q": "enqueued<-2h"}), &enq)

	var mismatch aqlErrorResp
	wMis := aqlGetJSON(t, h, tasksURL(map[string]string{"q": "state=pending error~timeout"}), &mismatch)

	var legacyStateMismatch aqlErrorResp
	wLegacy := aqlGetJSON(t, h, tasksURL(map[string]string{"q": "error~timeout", "state": "pending"}), &legacyStateMismatch)

	Convey("Given the structured 400 contract {error, position, hint}", t, func() {
		Convey("When the query asks for an enqueue age", func() {
			Convey("Then the §3.4 rejection is returned verbatim with a caret position", func() {
				So(wEnq.Code, ShouldEqual, 400)
				So(enq.Error, ShouldEqual, "asynq stores no enqueue timestamp")
				So(enq.Position, ShouldEqual, 0)
				So(enq.Hint, ShouldContainSubstring, "Try `pending_age>2h`.")
			})
		})
		Convey("When a field is used in a state that cannot answer it", func() {
			Convey("Then the rejection names the field, the states it works in, and is positioned at the clause", func() {
				So(wMis.Code, ShouldEqual, 400)
				So(mismatch.Error, ShouldContainSubstring, "`error`")
				So(mismatch.Position, ShouldEqual, len("state=pending "))
				So(mismatch.Hint, ShouldContainSubstring, "retry/archived")
			})
		})
		Convey("When the legacy state param conflicts with the query", func() {
			Convey("Then the same rejection shape applies", func() {
				So(wLegacy.Code, ShouldEqual, 400)
				So(legacyStateMismatch.Error, ShouldContainSubstring, "`error`")
			})
		})
	})
}

func TestStateCountsEndpoint(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})

	// Seed queue "scq": 4 pending (then 1 archived → 3 pending / 1 archived)
	// + 2 scheduled. A second queue adds fleet-wide mass.
	var firstID string
	for i := 0; i < 4; i++ {
		ti, err := env.client.Enqueue(asynq.NewTask("job:x", nil), asynq.Queue("scq"))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if i == 0 {
			firstID = ti.ID
		}
	}
	if err := env.insp.ArchiveTask("scq", firstID); err != nil {
		t.Fatalf("archiving: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := env.client.Enqueue(asynq.NewTask("job:x", nil), asynq.Queue("scq"), asynq.ProcessIn(time.Hour)); err != nil {
			t.Fatalf("seeding scheduled: %v", err)
		}
	}
	seedPending(t, env, "scq2", "job:y", `{}`, 5)

	type countsResp struct {
		Queue       string           `json:"queue"`
		Counts      map[string]int64 `json:"counts"`
		Source      string           `json:"source"`
		RefreshedAt string           `json:"refreshed_at"`
	}

	var single countsResp
	wSingle := aqlGetJSON(t, h, "/api/tasks/state_counts?queue=scq", &single)
	var fleet countsResp
	wFleet := aqlGetJSON(t, h, "/api/tasks/state_counts", &fleet)

	Convey("Given seeded tasks across states and queues", t, func() {
		Convey("When asking for one queue's state counts", func() {
			Convey("Then all seven states come back from one pipelined pass", func() {
				So(wSingle.Code, ShouldEqual, 200)
				So(single.Source, ShouldEqual, "live")
				So(single.Counts["pending"], ShouldEqual, 3)
				So(single.Counts["scheduled"], ShouldEqual, 2)
				So(single.Counts["archived"], ShouldEqual, 1)
				So(single.Counts["active"], ShouldEqual, 0)
				So(single.Counts["retry"], ShouldEqual, 0)
				So(single.Counts["completed"], ShouldEqual, 0)
				So(single.Counts["aggregating"], ShouldEqual, 0)
			})
		})
		Convey("When asking fleet-wide with no stats engine", func() {
			Convey("Then the live fallback sums every queue", func() {
				So(wFleet.Code, ShouldEqual, 200)
				So(fleet.Queue, ShouldEqual, "all")
				So(fleet.Counts["pending"], ShouldEqual, 8) // 3 + 5
				So(fleet.Counts["scheduled"], ShouldEqual, 2)
				So(fleet.Counts["archived"], ShouldEqual, 1)
			})
		})
	})
}

func TestJobScopeWithAql(t *testing.T) {
	env := newJobsTestEnv(t)
	h := env.newRouter(Options{RedisConnOpt: asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}})
	env.startTestRunner(t)

	// 6 pending tasks, 4 of them region=eu.
	seedPending(t, env, "aqljob", "job:a", `{"region":"eu"}`, 4)
	seedPending(t, env, "aqljob", "job:a", `{"region":"us"}`, 2)

	// Happy path: preview job scoped by AQL counts only the eu tasks.
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	doJSON(t, h, "POST", "/api/jobs", map[string]interface{}{
		"verb": "archive",
		"scope": map[string]interface{}{
			"queue": "aqljob", "state": "pending", "q": "", "meta": []string{},
			"aql": "meta.region=eu",
		},
		"reason": "phase-6 seam test",
	}, &created)

	var previewed struct {
		State  string `json:"state"`
		Counts struct {
			Candidates int64 `json:"candidates"`
		} `json:"counts"`
		PreviewComplete bool `json:"preview_complete"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		doJSON(t, h, "GET", "/api/jobs/"+created.ID, nil, &previewed)
		if previewed.PreviewComplete || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Phase 12 lifted the phase-6 env rejection: env-dependent predicates
	// are now valid job scopes (the runner evaluates them through the
	// aql.EnvBuilder session — execution covered by TestJobEnvScope).
	envBody := map[string]interface{}{
		"verb": "cancel",
		"scope": map[string]interface{}{
			"queue": "aqljob", "state": "active", "q": "", "meta": []string{},
			"aql": "orphaned",
		},
		"reason": "env scopes are valid since phase 12",
	}
	var envCreated struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	envW := doJSON(t, h, "POST", "/api/jobs", envBody, &envCreated)

	// A malformed AQL clause is still rejected with the positioned error.
	badBody := map[string]interface{}{
		"verb": "archive",
		"scope": map[string]interface{}{
			"queue": "aqljob", "state": "pending", "q": "", "meta": []string{},
			"aql": "enqueued<-2h",
		},
		"reason": "should be rejected",
	}
	badW := doJSON(t, h, "POST", "/api/jobs", badBody, nil)
	var rejected aqlErrorResp
	_ = json.Unmarshal(badW.Body.Bytes(), &rejected)

	Convey("Given the jobs Matcher seam", t, func() {
		Convey("When a preview job carries an AQL scope", func() {
			Convey("Then enumeration applies the compiled matcher", func() {
				So(previewed.PreviewComplete, ShouldBeTrue)
				So(previewed.Counts.Candidates, ShouldEqual, 4)
			})
		})
		Convey("When the AQL scope needs live worker/lease data (phase 12)", func() {
			Convey("Then create is accepted — the runner's env session evaluates it", func() {
				So(envW.Code, ShouldEqual, 201)
				So(envCreated.ID, ShouldNotBeEmpty)
			})
		})
		Convey("When the AQL scope is unanswerable", func() {
			Convey("Then create is still refused with the structured rejection", func() {
				So(badW.Code, ShouldEqual, 400)
				So(rejected.Error, ShouldNotBeEmpty)
			})
		})
	})
}
