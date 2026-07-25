package asynqmon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Integration tests for the bulk-job runner, its HTTP surface, identity
// resolution, and the audit stream — against a real Redis on 127.0.0.1:6379
// DB 12. (The stats package tests own DB 14, the fleet handler tests own DB
// 13; only DB 12 is flushed here.) Fixtures are seeded through the real
// asynq client so key layouts are authentic. Skipped when Redis does not
// answer PING.
//
// Convey discipline: goconvey re-executes ancestor blocks once per leaf, so
// every flow that mutates queues or claims leases runs imperatively BEFORE
// the Convey tree; the tree itself only reads captured results (the same
// pattern stats_handlers_test.go uses).
//
// The job/audit JSON field names asserted here are FROZEN frontend contracts
// (ui/src/api.ts) — changes require frontend sign-off.
// ****************************************************************************

const (
	jobsTestRedisAddr = "127.0.0.1:6379"
	jobsTestRedisDB   = 12
)

type jobsTestEnv struct {
	rc     *redis.Client
	client *asynq.Client
	insp   *asynq.Inspector
	store  *jobs.Store
}

// newJobsTestEnv flushes DB 12 and returns clients wired to it.
func newJobsTestEnv(t *testing.T) *jobsTestEnv {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", jobsTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	opt := asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})
	return &jobsTestEnv{rc: rc, client: client, insp: insp, store: jobs.NewStore(rc)}
}

// newRouter builds the real router (actor middleware included; read-only
// filter when opts.ReadOnly) against the test env's Redis. No stats engine
// and no runner — runners are started explicitly per test.
func (e *jobsTestEnv) newRouter(opts Options) http.Handler {
	return muxRouter(opts, e.rc, e.insp, nil, nil)
}

// startTestRunner starts a fast-polling runner wired to DB 12.
func (e *jobsTestEnv) startTestRunner(t *testing.T) *jobs.Runner {
	t.Helper()
	runner := jobs.NewRunner(jobs.Config{
		RedisClient:  e.rc,
		Inspector:    e.insp,
		PollInterval: 50 * time.Millisecond,
		PreviewSleep: time.Millisecond,
		Logf:         t.Logf,
	})
	runner.Start(context.Background())
	t.Cleanup(runner.Stop)
	return runner
}

// doJSON performs one request against the router and decodes the response.
func doJSON(t *testing.T, h http.Handler, method, url string, body interface{}, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, url, &buf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding %s %s response: %v\nbody: %s", method, url, err, w.Body.String())
		}
	}
	return w
}

// awaitJob polls the store until cond holds or a deadline passes.
func awaitJob(t *testing.T, store *jobs.Store, id string, what string, cond func(*jobs.Job) bool) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, err := store.Get(context.Background(), id)
		if err == nil && cond(j) {
			return j
		}
		time.Sleep(25 * time.Millisecond)
	}
	j, _ := store.Get(context.Background(), id)
	t.Fatalf("timed out waiting for job %s: %s (job: %+v)", id, what, j)
	return nil
}

func seedPending(t *testing.T, e *jobsTestEnv, qname, typ string, payload string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.client.Enqueue(asynq.NewTask(typ, []byte(payload)), asynq.Queue(qname)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
}

// ----------------------------------------------------------------------------
// The §4.3 flow end to end: preview → mutate behind its back → execute →
// re-verify (skipped count) → audit. The flow runs imperatively; Convey
// blocks only read the captured results.
// ----------------------------------------------------------------------------

func TestBulkJobPreviewExecuteRoundTrip(t *testing.T) {
	env := newJobsTestEnv(t)
	env.startTestRunner(t)
	router := env.newRouter(Options{})

	seedPending(t, env, "bulk", "email:send", `{"kind":"a"}`, 8)
	seedPending(t, env, "bulk", "email:send", `{"kind":"b"}`, 3)

	// Create the job (archive every pending kind=a task in queue "bulk").
	var created struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Phase     string `json:"phase"`
		Actor     string `json:"actor"`
		CostClass string `json:"cost_class"`
	}
	wCreate := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
		"verb":     "archive",
		"scope":    map[string]interface{}{"queue": "bulk", "state": "pending", "meta": []string{"kind:a"}},
		"reason":   "roundtrip test",
		"throttle": 1000,
	}, &created)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("creating job: %d %s", wCreate.Code, wCreate.Body.String())
	}

	// Wait for the runner's preview, then read the full job.
	awaitJob(t, env.store, created.ID, "preview_ready",
		func(j *jobs.Job) bool { return j.State == jobs.StatePreviewReady })
	var preview struct {
		Counts          jobs.Counts      `json:"counts"`
		PreviewComplete bool             `json:"preview_complete"`
		CostListLen     int64            `json:"cost_list_len"`
		Sample          []jobs.SampleRow `json:"sample"`
	}
	doJSON(t, router, "GET", "/api/jobs/"+created.ID, nil, &preview)
	if len(preview.Sample) == 0 {
		t.Fatalf("expected a preview sample, got none")
	}

	// Mutate a candidate behind the job's back, then execute.
	if err := env.insp.DeleteTask("bulk", preview.Sample[0].ID); err != nil {
		t.Fatalf("deleting sample task: %v", err)
	}
	var exec struct {
		State string `json:"state"`
		Phase string `json:"phase"`
	}
	wExec := doJSON(t, router, "POST", "/api/jobs/"+created.ID+"/execute",
		map[string]interface{}{"proceed_on_partial": false}, &exec)
	if wExec.Code != http.StatusOK {
		t.Fatalf("executing job: %d %s", wExec.Code, wExec.Body.String())
	}
	done := awaitJob(t, env.store, created.ID, "done",
		func(j *jobs.Job) bool { return j.State == jobs.StateDone })

	var audit struct {
		Entries []jobs.AuditEntry `json:"entries"`
	}
	doJSON(t, router, "GET", "/api/audit", nil, &audit)

	Convey("Given 8 kind=a and 3 kind=b pending tasks and an archive job scoped to kind=a", t, func() {
		Convey("When the job was created", func() {
			Convey("Then it started previewing, attributed, cost-classed as a LIST removal", func() {
				So(created.State, ShouldEqual, "previewing")
				So(created.Phase, ShouldEqual, "preview")
				So(created.Actor, ShouldStartWith, "anonymous@")
				So(created.CostClass, ShouldEqual, "list_removal") // archive on pending = LREM per task
			})
		})

		Convey("When the runner finished the preview", func() {
			Convey("Then it counted exactly the 8 matching candidates with a capped sample and the list length", func() {
				So(preview.PreviewComplete, ShouldBeTrue)
				So(preview.Counts.Candidates, ShouldEqual, 8)
				So(len(preview.Sample), ShouldBeLessThanOrEqualTo, 10)
				So(preview.Sample[0].Queue, ShouldEqual, "bulk")
				So(preview.Sample[0].Payload, ShouldContainSubstring, `"kind":"a"`)
				So(preview.CostListLen, ShouldEqual, 11) // whole pending list, fetched once
			})
		})

		Convey("When one candidate was deleted between preview and execute", func() {
			Convey("Then execution re-verified each task: 7 acted, the mutated one skipped, none failed", func() {
				So(exec.Phase, ShouldEqual, "execute")
				So(exec.State, ShouldEqual, "running")
				So(done.Counts.Acted, ShouldEqual, 7)
				So(done.Counts.Skipped, ShouldEqual, 1)
				So(done.Counts.Failed, ShouldEqual, 0)
				So(done.FinishedAt.IsZero(), ShouldBeFalse)
			})

			Convey("Then Redis agrees: 7 archived, only the 3 kind=b tasks left pending", func() {
				archived, err := env.insp.ListArchivedTasks("bulk", asynq.PageSize(100))
				So(err, ShouldBeNil)
				So(archived, ShouldHaveLength, 7)
				pending, err := env.insp.ListPendingTasks("bulk", asynq.PageSize(100))
				So(err, ShouldBeNil)
				So(pending, ShouldHaveLength, 3)
				for _, ti := range pending {
					So(string(ti.Payload), ShouldContainSubstring, `"kind":"b"`)
				}
			})

			Convey("Then the audit stream holds creation and completion, newest first", func() {
				So(len(audit.Entries), ShouldBeGreaterThanOrEqualTo, 2)
				So(audit.Entries[0].Event, ShouldEqual, jobs.AuditJobFinished)
				So(audit.Entries[0].JobID, ShouldEqual, created.ID)
				So(audit.Entries[0].Acted, ShouldEqual, 7)
				So(audit.Entries[0].Skipped, ShouldEqual, 1)
				So(audit.Entries[0].PreviewCount, ShouldEqual, 8)
				So(audit.Entries[0].Reason, ShouldEqual, "roundtrip test")
				last := audit.Entries[len(audit.Entries)-1]
				So(last.Event, ShouldEqual, jobs.AuditJobCreated)
				So(last.Actor, ShouldStartWith, "anonymous@")
				So(last.Scope.State, ShouldEqual, "pending")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// The execute gate (§4.3 step 2). No runner runs here, so previews stay
// incomplete deterministically. Each rerun path creates its own fresh job.
// ----------------------------------------------------------------------------

func TestBulkJobExecuteGate(t *testing.T) {
	env := newJobsTestEnv(t)
	router := env.newRouter(Options{})

	createJob := func(verb, state string) string {
		t.Helper()
		var created struct {
			ID string `json:"id"`
		}
		w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
			"verb":   verb,
			"scope":  map[string]interface{}{"queue": "bulk", "state": state},
			"reason": "gate test",
		}, &created)
		if w.Code != http.StatusCreated {
			t.Fatalf("creating %s/%s job: %d %s", verb, state, w.Code, w.Body.String())
		}
		return created.ID
	}

	Convey("Given jobs whose previews have not completed (no runner claims them)", t, func() {
		Convey("When executing a delete job", func() {
			id := createJob("delete", "pending")

			Convey("Then it is refused without a completed preview — even with proceed_on_partial", func() {
				w := doJSON(t, router, "POST", "/api/jobs/"+id+"/execute", map[string]interface{}{}, nil)
				So(w.Code, ShouldEqual, http.StatusPreconditionFailed)
				w = doJSON(t, router, "POST", "/api/jobs/"+id+"/execute",
					map[string]interface{}{"proceed_on_partial": true}, nil)
				So(w.Code, ShouldEqual, http.StatusPreconditionFailed)
				So(w.Body.String(), ShouldContainSubstring, "delete requires a completed preview")
			})
		})

		Convey("When executing a run job", func() {
			id := createJob("run", "scheduled")

			Convey("Then it is refused until the partial count is acknowledged", func() {
				w := doJSON(t, router, "POST", "/api/jobs/"+id+"/execute", map[string]interface{}{}, nil)
				So(w.Code, ShouldEqual, http.StatusPreconditionFailed)
				So(w.Body.String(), ShouldContainSubstring, "proceed_on_partial")
			})

			Convey("Then with the explicit acknowledgment it moves to running, and a repeat conflicts", func() {
				var exec struct {
					State            string `json:"state"`
					ProceedOnPartial bool   `json:"proceed_on_partial"`
				}
				w := doJSON(t, router, "POST", "/api/jobs/"+id+"/execute",
					map[string]interface{}{"proceed_on_partial": true}, &exec)
				So(w.Code, ShouldEqual, http.StatusOK)
				So(exec.State, ShouldEqual, "running")
				So(exec.ProceedOnPartial, ShouldBeTrue)

				w2 := doJSON(t, router, "POST", "/api/jobs/"+id+"/execute",
					map[string]interface{}{"proceed_on_partial": true}, nil)
				So(w2.Code, ShouldEqual, http.StatusConflict)
			})
		})

		Convey("When creating jobs with invalid inputs", func() {
			Convey("Then a missing reason is rejected", func() {
				w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
					"verb":  "run",
					"scope": map[string]interface{}{"queue": "bulk", "state": "retry"},
				}, nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
				So(w.Body.String(), ShouldContainSubstring, "reason")
			})
			Convey("Then a verb the state does not support is rejected", func() {
				w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
					"verb":   "delete",
					"scope":  map[string]interface{}{"queue": "bulk", "state": "active"},
					"reason": "x",
				}, nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then an off-menu throttle is rejected", func() {
				w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
					"verb":     "run",
					"scope":    map[string]interface{}{"queue": "bulk", "state": "retry"},
					"reason":   "x",
					"throttle": 7,
				}, nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then an unknown job 404s", func() {
				w := doJSON(t, router, "GET", "/api/jobs/jb_nope", nil, nil)
				So(w.Code, ShouldEqual, http.StatusNotFound)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Fencing (§5.13): a second claimer supersedes the first; the stale token
// can no longer write. Claims are stateful, so the whole sequence runs
// imperatively and Convey only narrates the captured results.
// ----------------------------------------------------------------------------

func TestBulkJobFencing(t *testing.T) {
	env := newJobsTestEnv(t)
	ctx := context.Background()

	j := &jobs.Job{
		ID: jobs.NewJobID(), Verb: jobs.VerbRun,
		Scope: jobs.Scope{Queue: "bulk", State: "retry"},
		Phase: jobs.PhasePreview, State: jobs.StatePreviewing,
		Throttle: 200, Reason: "fencing test", Actor: "test",
		CostClass: jobs.CostCheap, CreatedAt: time.Now(),
	}
	if err := env.store.Create(ctx, j); err != nil {
		t.Fatalf("creating job: %v", err)
	}

	// A claims with a short lease.
	tokenA, errA := env.store.Claim(ctx, j.ID, "replica-A", 100*time.Millisecond)
	// B tries while A still holds the lease.
	tokenBEarly, errBEarly := env.store.Claim(ctx, j.ID, "replica-B", 100*time.Millisecond)
	// A's lease expires (simulated partition/pause); B reclaims.
	time.Sleep(150 * time.Millisecond)
	tokenB, errB := env.store.Claim(ctx, j.ID, "replica-B", time.Minute)
	// The stale claimer A attempts a batch write; then B writes.
	okA, wErrA := env.store.WriteProgress(ctx, j.ID, tokenA, jobs.ProgressWrite{Fields: map[string]string{"acted": "999"}})
	okB, wErrB := env.store.WriteProgress(ctx, j.ID, tokenB, jobs.ProgressWrite{Fields: map[string]string{"acted": "7"}})
	fresh, gErr := env.store.Get(ctx, j.ID)

	Convey("Given a job claimed by replica A on a short lease", t, func() {
		Convey("When A claimed it", func() {
			Convey("Then A holds a positive fencing token and B's concurrent claim is refused", func() {
				So(errA, ShouldBeNil)
				So(tokenA, ShouldBeGreaterThan, 0)
				So(errBEarly, ShouldBeNil)
				So(tokenBEarly, ShouldEqual, 0)
			})
		})

		Convey("When A's lease expired and B reclaimed the job", func() {
			Convey("Then B's fencing token supersedes A's", func() {
				So(errB, ShouldBeNil)
				So(tokenB, ShouldBeGreaterThan, tokenA)
			})

			Convey("Then A's stale-token batch write is rejected while B's lands", func() {
				So(wErrA, ShouldBeNil)
				So(okA, ShouldBeFalse)
				So(wErrB, ShouldBeNil)
				So(okB, ShouldBeTrue)
				So(gErr, ShouldBeNil)
				So(fresh.Counts.Acted, ShouldEqual, 7) // B's 7, never A's 999
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Pause / resume / cancel control plane.
// ----------------------------------------------------------------------------

func TestBulkJobControls(t *testing.T) {
	env := newJobsTestEnv(t)
	env.startTestRunner(t)
	router := env.newRouter(Options{})

	seedPending(t, env, "ctl", "x", `{}`, 3)

	// A preview_ready job, canceled while dormant — imperative, then narrated.
	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
		"verb":   "archive",
		"scope":  map[string]interface{}{"queue": "ctl", "state": "pending"},
		"reason": "controls test",
	}, &created)
	awaitJob(t, env.store, created.ID, "preview_ready",
		func(j *jobs.Job) bool { return j.State == jobs.StatePreviewReady })
	var canceled struct {
		State      string `json:"state"`
		FinishedAt string `json:"finished_at"`
	}
	wCancel := doJSON(t, router, "POST", "/api/jobs/"+created.ID+"/cancel", nil, &canceled)
	wPauseAfter := doJSON(t, router, "POST", "/api/jobs/"+created.ID+"/pause", nil, nil)
	wCancelAgain := doJSON(t, router, "POST", "/api/jobs/"+created.ID+"/cancel", nil, nil)

	Convey("Given a job whose preview completed and now sits preview_ready", t, func() {
		Convey("When it was canceled while dormant", func() {
			Convey("Then it finalized to canceled immediately with a finish time", func() {
				So(wCancel.Code, ShouldEqual, http.StatusOK)
				So(canceled.State, ShouldEqual, "canceled")
				So(canceled.FinishedAt, ShouldNotBeEmpty)
			})
			Convey("Then further control requests conflict", func() {
				So(wPauseAfter.Code, ShouldEqual, http.StatusConflict)
				So(wCancelAgain.Code, ShouldEqual, http.StatusConflict)
			})
		})

		Convey("When the jobs list is read", func() {
			var list struct {
				Jobs []struct {
					ID    string `json:"id"`
					State string `json:"state"`
				} `json:"jobs"`
			}
			w := doJSON(t, router, "GET", "/api/jobs?limit=10", nil, &list)

			Convey("Then it returns the canceled job, newest first", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(len(list.Jobs), ShouldBeGreaterThanOrEqualTo, 1)
				So(list.Jobs[0].ID, ShouldEqual, created.ID)
				So(list.Jobs[0].State, ShouldEqual, "canceled")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Read-only mode + the §5.11 identity chain.
// ----------------------------------------------------------------------------

func TestJobsReadOnlyMode(t *testing.T) {
	env := newJobsTestEnv(t)
	router := env.newRouter(Options{ReadOnly: true})

	Convey("Given the API in read-only mode", t, func() {
		Convey("When a job creation is attempted", func() {
			w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
				"verb": "run", "scope": map[string]interface{}{"state": "retry"}, "reason": "x",
			}, nil)

			Convey("Then it is refused with 405 like every other mutation", func() {
				So(w.Code, ShouldEqual, http.StatusMethodNotAllowed)
			})
		})

		Convey("When jobs and audit are read", func() {
			wJobs := doJSON(t, router, "GET", "/api/jobs", nil, nil)
			wAudit := doJSON(t, router, "GET", "/api/audit", nil, nil)

			Convey("Then reads still work", func() {
				So(wJobs.Code, ShouldEqual, http.StatusOK)
				So(wAudit.Code, ShouldEqual, http.StatusOK)
			})
		})
	})
}

func TestJobsIdentityChain(t *testing.T) {
	env := newJobsTestEnv(t)

	createWith := func(router http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]interface{}{
			"verb": "run", "scope": map[string]interface{}{"queue": "q", "state": "retry"}, "reason": "identity test",
		})
		req := httptest.NewRequest("POST", "/api/jobs", bytes.NewReader(body))
		if mutate != nil {
			mutate(req)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	actorOf := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var resp struct {
			Actor string `json:"actor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding create response: %v\nbody: %s", err, w.Body.String())
		}
		return resp.Actor
	}

	Convey("Given the §5.11 actor resolution chain", t, func() {
		Convey("When a trusted proxy sends the auth header", func() {
			// httptest requests originate from 192.0.2.1 (RFC 5737 TEST-NET).
			router := env.newRouter(Options{AuthHeader: "X-Auth-Request-User", TrustedProxies: []string{"192.0.2.0/24"}})
			w := createWith(router, func(r *http.Request) { r.Header.Set("X-Auth-Request-User", "zac@ops") })

			Convey("Then the header names the actor", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				So(actorOf(w), ShouldEqual, "zac@ops")
			})
		})

		Convey("When the same header arrives from outside the trusted CIDRs", func() {
			router := env.newRouter(Options{AuthHeader: "X-Auth-Request-User", TrustedProxies: []string{"10.0.0.0/8"}})
			w := createWith(router, func(r *http.Request) {
				r.Header.Set("X-Auth-Request-User", "spoofed@evil")
				r.SetBasicAuth("mira", "pw")
			})

			Convey("Then the header is ignored and basic-auth wins", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				So(actorOf(w), ShouldEqual, "mira")
			})
		})

		Convey("When identity is required and none is supplied", func() {
			router := env.newRouter(Options{RequireIdentity: true})
			w := createWith(router, nil)

			Convey("Then the mutation is refused with a 403 JSON error", func() {
				So(w.Code, ShouldEqual, http.StatusForbidden)
				So(w.Header().Get("Content-Type"), ShouldContainSubstring, "application/json")
				So(w.Body.String(), ShouldContainSubstring, "identity required")
			})

			Convey("Then read requests still pass", func() {
				wr := doJSON(t, router, "GET", "/api/jobs", nil, nil)
				So(wr.Code, ShouldEqual, http.StatusOK)
			})
		})

		Convey("When nothing identifies the caller and identity is not required", func() {
			router := env.newRouter(Options{})
			w := createWith(router, nil)

			Convey("Then the actor falls back to anonymous@<ip>", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				So(actorOf(w), ShouldEqual, "anonymous@192.0.2.1")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Frozen wire contract.
// ----------------------------------------------------------------------------

func TestJobJSONContract(t *testing.T) {
	env := newJobsTestEnv(t)
	router := env.newRouter(Options{})

	w := doJSON(t, router, "POST", "/api/jobs", map[string]interface{}{
		"verb": "archive", "scope": map[string]interface{}{"queue": "q", "state": "retry"}, "reason": "contract",
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating job: %d %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding create response: %v", err)
	}
	id, _ := body["id"].(string)
	var detail map[string]interface{}
	doJSON(t, router, "GET", fmt.Sprintf("/api/jobs/%s", id), nil, &detail)

	Convey("Given a freshly created job", t, func() {
		Convey("When the create/detail payloads are inspected", func() {
			Convey("Then the field names match the frontend contract exactly", func() {
				for _, key := range []string{
					"id", "verb", "scope", "phase", "state", "throttle", "reason", "actor",
					"counts", "cost_class", "cost_list_len", "preview_complete",
					"proceed_on_partial", "created_at", "started_at", "finished_at",
					"fence", "error", "failures_overflow", "ctl_pending",
				} {
					_, ok := body[key]
					So(ok, ShouldBeTrue)
				}
				for _, key := range []string{"sample", "failures", "failures_total"} {
					_, ok := detail[key]
					So(ok, ShouldBeTrue)
				}
				counts := body["counts"].(map[string]interface{})
				for _, key := range []string{"candidates", "acted", "skipped", "failed"} {
					_, ok := counts[key]
					So(ok, ShouldBeTrue)
				}
				scope := body["scope"].(map[string]interface{})
				for _, key := range []string{"queue", "state", "q", "meta"} {
					_, ok := scope[key]
					So(ok, ShouldBeTrue)
				}
			})
		})
	})
}
