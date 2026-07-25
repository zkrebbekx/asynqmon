package asynqmon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// Integration tests for the flag-gated enqueue capability (§5.10) — against
// a real Redis on 127.0.0.1:6379 DB 8. (DBs 9-14 are owned by the errsig,
// scheduler, coverage, jobs, stats-handler and stats-engine suites; only DB
// 8 is flushed here.) Round-trips go through the real asynq client and
// Inspector so key layouts are authentic. Skipped when Redis does not
// answer PING.
//
// Convey discipline: goconvey re-executes ancestor blocks once per leaf, so
// every flow that mutates queues runs imperatively BEFORE the Convey tree;
// the tree itself only reads captured results (the jobs_handlers_test.go
// pattern).
//
// The enqueue request/response field names asserted here are FROZEN frontend
// contracts (ui/src/api.ts EnqueueTaskRequest / TaskInfo).
// ****************************************************************************

const (
	enqueueTestRedisAddr = "127.0.0.1:6379"
	enqueueTestRedisDB   = 8
)

type enqueueTestEnv struct {
	rc     *redis.Client
	client *asynq.Client
	insp   *asynq.Inspector
	store  *jobs.Store
}

// newEnqueueTestEnv flushes DB 8 and returns clients wired to it.
func newEnqueueTestEnv(t *testing.T) *enqueueTestEnv {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: enqueueTestRedisAddr, DB: enqueueTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", enqueueTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", enqueueTestRedisDB, err)
	}
	opt := asynq.RedisClientOpt{Addr: enqueueTestRedisAddr, DB: enqueueTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})
	return &enqueueTestEnv{rc: rc, client: client, insp: insp, store: jobs.NewStore(rc)}
}

// newRouter builds the real router (actor middleware, read-only filter, and
// the §5.10 route block included) against the test env's Redis. The env's
// asynq client is injected as the enqueue client so nothing leaks per test.
func (e *enqueueTestEnv) newRouter(opts Options) http.Handler {
	opts.enqueueClient = e.client
	return muxRouter(opts, e.rc, e.insp, nil, nil)
}

func enqueueURL(qname string) string { return "/api/queues/" + qname + "/tasks" }

// ----------------------------------------------------------------------------
// Success round-trip: POST → 201 taskInfo → Inspector GetTaskInfo agrees →
// audit entry written. Runs imperatively; Convey blocks read the results.
// ----------------------------------------------------------------------------

func TestEnqueueTaskRoundTrip(t *testing.T) {
	env := newEnqueueTestEnv(t)
	router := env.newRouter(Options{EnableEnqueue: true})

	// Immediate enqueue with the full option set.
	var created taskInfo
	w := doJSON(t, router, "POST", enqueueURL("emails"), map[string]interface{}{
		"type":               "email:send",
		"payload":            `{"to":"ops@example.com"}`,
		"max_retry":          7,
		"timeout_seconds":    90,
		"retention_seconds":  3600,
		"unique_ttl_seconds": 60,
		"reason":             "manual resend",
	}, &created)
	var inspected *asynq.TaskInfo
	var inspectErr error
	if w.Code == http.StatusCreated {
		inspected, inspectErr = env.insp.GetTaskInfo("emails", created.ID)
	}

	// Scheduled enqueue (process_in) with an empty payload.
	var scheduled taskInfo
	ws := doJSON(t, router, "POST", enqueueURL("emails"), map[string]interface{}{
		"type":               "email:digest",
		"payload":            "",
		"process_in_seconds": 3600,
	}, &scheduled)

	// Binary payload via base64 mode.
	rawBytes := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	var binary taskInfo
	wb := doJSON(t, router, "POST", enqueueURL("emails"), map[string]interface{}{
		"type":           "blob:store",
		"payload":        base64.StdEncoding.EncodeToString(rawBytes),
		"payload_base64": true,
	}, &binary)
	var binaryInspected *asynq.TaskInfo
	if wb.Code == http.StatusCreated {
		binaryInspected, _ = env.insp.GetTaskInfo("emails", binary.ID)
	}

	audits, auditErr := env.store.ReadAudit(context.Background(), 10)

	Convey("Given an enqueue-enabled router and three POSTed tasks", t, func() {
		Convey("When a task was enqueued with the full option set", func() {
			Convey("Then it answered 201 with the created taskInfo", func() {
				So(w.Code, ShouldEqual, http.StatusCreated)
				So(created.ID, ShouldNotBeEmpty)
				So(created.Queue, ShouldEqual, "emails")
				So(created.Type, ShouldEqual, "email:send")
				So(created.State, ShouldEqual, "pending")
				So(created.MaxRetry, ShouldEqual, 7)
				So(created.Timeout, ShouldEqual, 90)
			})
			Convey("Then the Inspector round-trip agrees on every mapped option", func() {
				So(inspectErr, ShouldBeNil)
				So(inspected, ShouldNotBeNil)
				So(inspected.Type, ShouldEqual, "email:send")
				So(string(inspected.Payload), ShouldEqual, `{"to":"ops@example.com"}`)
				So(inspected.MaxRetry, ShouldEqual, 7)
				So(inspected.Timeout, ShouldEqual, 90*time.Second)
				So(inspected.Retention, ShouldEqual, time.Hour)
				So(inspected.State, ShouldEqual, asynq.TaskStatePending)
			})
		})
		Convey("When a task was scheduled via process_in_seconds with an empty payload", func() {
			Convey("Then it landed in the scheduled state with a future next_process_at", func() {
				So(ws.Code, ShouldEqual, http.StatusCreated)
				So(scheduled.State, ShouldEqual, "scheduled")
				npa, err := time.Parse(time.RFC3339, scheduled.NextProcessAt)
				So(err, ShouldBeNil)
				So(npa.After(time.Now().Add(50*time.Minute)), ShouldBeTrue)
			})
		})
		Convey("When a binary payload was enqueued in base64 mode", func() {
			Convey("Then the stored payload bytes are the decoded binary", func() {
				So(wb.Code, ShouldEqual, http.StatusCreated)
				So(binaryInspected, ShouldNotBeNil)
				So(binaryInspected.Payload, ShouldResemble, rawBytes)
			})
		})
		Convey("When the audit stream is read back", func() {
			Convey("Then each enqueue wrote an attributed entry: verb enqueue, scope queue/type, count 1", func() {
				So(auditErr, ShouldBeNil)
				So(len(audits), ShouldEqual, 3)
				// Newest first: blob:store, email:digest, email:send.
				So(audits[2].Event, ShouldEqual, jobs.AuditTaskEnqueued)
				So(audits[2].Verb, ShouldEqual, "enqueue")
				So(audits[2].Actor, ShouldStartWith, "anonymous@")
				So(audits[2].Scope.Queue, ShouldEqual, "emails")
				So(audits[2].Scope.Q, ShouldEqual, "email:send")
				So(audits[2].TaskID, ShouldEqual, created.ID)
				So(audits[2].Acted, ShouldEqual, 1)
				So(audits[2].Reason, ShouldEqual, "manual resend")
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Gating: flag off → 403; read-only → 403 (JSON, with reasons); the identity
// chain still applies when enabled; /api/features reports the capability.
// ----------------------------------------------------------------------------

func TestEnqueueGating(t *testing.T) {
	env := newEnqueueTestEnv(t)

	body := map[string]interface{}{"type": "email:send", "payload": "{}"}

	disabledRouter := env.newRouter(Options{})
	wDisabled := doJSON(t, disabledRouter, "POST", enqueueURL("emails"), body, nil)

	roRouter := env.newRouter(Options{EnableEnqueue: true, ReadOnly: true})
	wRO := doJSON(t, roRouter, "POST", enqueueURL("emails"), body, nil)

	identityRouter := env.newRouter(Options{EnableEnqueue: true, RequireIdentity: true})
	wAnon := doJSON(t, identityRouter, "POST", enqueueURL("emails"), body, nil)

	readFeatures := func(h http.Handler) (int, featuresResponse) {
		var f featuresResponse
		w := doJSON(t, h, "GET", "/api/features", nil, &f)
		return w.Code, f
	}
	enabledRouter := env.newRouter(Options{EnableEnqueue: true})
	fEnabledCode, fEnabled := readFeatures(enabledRouter)
	fDisabledCode, fDisabled := readFeatures(disabledRouter)
	fROCode, fRO := readFeatures(roRouter)

	Convey("Given routers in each gating configuration", t, func() {
		Convey("When enqueue is not enabled (the default)", func() {
			Convey("Then POST answers 403 JSON pointing at --enable-enqueue", func() {
				So(wDisabled.Code, ShouldEqual, http.StatusForbidden)
				So(wDisabled.Header().Get("Content-Type"), ShouldContainSubstring, "application/json")
				So(wDisabled.Body.String(), ShouldContainSubstring, "--enable-enqueue")
			})
		})
		Convey("When the flag is on but the replica is read-only", func() {
			Convey("Then POST answers 403 JSON naming read-only mode, not a bare 405", func() {
				So(wRO.Code, ShouldEqual, http.StatusForbidden)
				So(wRO.Header().Get("Content-Type"), ShouldContainSubstring, "application/json")
				So(wRO.Body.String(), ShouldContainSubstring, "read-only")
			})
		})
		Convey("When identities are required and the request carries none", func() {
			Convey("Then the identity chain refuses the mutation with 403", func() {
				So(wAnon.Code, ShouldEqual, http.StatusForbidden)
				So(wAnon.Body.String(), ShouldContainSubstring, "identity required")
			})
		})
		Convey("When the frontend probes GET /api/features", func() {
			Convey("Then enqueue is reported true only when enabled and writable", func() {
				So(fEnabledCode, ShouldEqual, http.StatusOK)
				So(fEnabled.Features.Enqueue, ShouldBeTrue)
				So(fDisabledCode, ShouldEqual, http.StatusOK)
				So(fDisabled.Features.Enqueue, ShouldBeFalse)
				So(fROCode, ShouldEqual, http.StatusOK)
				So(fRO.Features.Enqueue, ShouldBeFalse)
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Features shape: GET /api/features carries the Flow view's correlation-key
// list (§3.5) next to the capability flags. Exercised on the handler func
// directly — the wire shape needs no Redis.
// ----------------------------------------------------------------------------

func TestFeaturesCorrelationKeys(t *testing.T) {
	probe := func(keys []string) featuresResponse {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/features", nil)
		newFeaturesHandlerFunc(false, normalizeCorrelationKeys(keys))(w, r)
		var f featuresResponse
		if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
			t.Fatalf("decoding features response: %v", err)
		}
		return f
	}

	defResp := probe(nil)
	customResp := probe([]string{"order_ref", "trace_id"})
	messyResp := probe([]string{" order_ref ", "", "trace_id", "order_ref", "  "})
	emptyResp := probe([]string{"", "  "})

	Convey("Given the GET /api/features handler", t, func() {
		Convey("When no correlation keys are configured", func() {
			Convey("Then correlation_keys carries the default list in priority order", func() {
				So(defResp.CorrelationKeys, ShouldResemble,
					[]string{"trace_id", "correlation_id", "request_id"})
			})
		})
		Convey("When a custom list is configured", func() {
			Convey("Then correlation_keys mirrors it verbatim", func() {
				So(customResp.CorrelationKeys, ShouldResemble, []string{"order_ref", "trace_id"})
			})
		})
		Convey("When the list carries whitespace, empties, and duplicates", func() {
			Convey("Then entries are trimmed, empties dropped, and dupes keep first slot", func() {
				So(messyResp.CorrelationKeys, ShouldResemble, []string{"order_ref", "trace_id"})
			})
		})
		Convey("When every configured entry is blank", func() {
			Convey("Then the default list applies — a stray flag can't disable the Flow view", func() {
				So(emptyResp.CorrelationKeys, ShouldResemble,
					[]string{"trace_id", "correlation_id", "request_id"})
			})
		})
	})
}

// ----------------------------------------------------------------------------
// Validation 400s (field-specific messages) and the unique-conflict 409.
// ----------------------------------------------------------------------------

func TestEnqueueValidationAndConflicts(t *testing.T) {
	env := newEnqueueTestEnv(t)
	router := env.newRouter(Options{EnableEnqueue: true})

	// post returns the status code and the decoded {"error": ...} message
	// (the encoder HTML-escapes <, > in the raw body).
	post := func(body map[string]interface{}) (int, string) {
		w := doJSON(t, router, "POST", enqueueURL("emails"), body, nil)
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return w.Code, e.Error
	}

	missingTypeCode, missingTypeBody := post(map[string]interface{}{"payload": "{}"})
	badB64Code, badB64Body := post(map[string]interface{}{
		"type": "t", "payload": "not-*-base64", "payload_base64": true,
	})
	oversizeCode, oversizeBody := post(map[string]interface{}{
		"type": "t", "payload": strings.Repeat("x", maxEnqueuePayloadBytes+1),
	})
	negRetryCode, negRetryBody := post(map[string]interface{}{"type": "t", "max_retry": -1})
	hugeTimeoutCode, hugeTimeoutBody := post(map[string]interface{}{
		"type": "t", "timeout_seconds": maxEnqueueTimeoutSeconds + 1,
	})
	zeroUniqueCode, zeroUniqueBody := post(map[string]interface{}{"type": "t", "unique_ttl_seconds": 0})
	badDeadlineCode, badDeadlineBody := post(map[string]interface{}{"type": "t", "deadline": "tomorrow"})
	bothScheduleCode, bothScheduleBody := post(map[string]interface{}{
		"type": "t", "process_at": time.Now().Add(time.Hour).Format(time.RFC3339), "process_in_seconds": 60,
	})
	farFutureCode, farFutureBody := post(map[string]interface{}{
		"type": "t", "process_at": time.Now().Add(2 * 365 * 24 * time.Hour).Format(time.RFC3339),
	})
	unknownFieldCode, unknownFieldBody := post(map[string]interface{}{"type": "t", "priority": 3})

	// Unique conflict: the first enqueue takes the uniqueness lock, the
	// identical second must answer 409 (asynq.ErrDuplicateTask).
	uniq := map[string]interface{}{
		"type": "report:build", "payload": `{"day":"2026-07-25"}`, "unique_ttl_seconds": 300,
	}
	firstUniqueCode, _ := post(uniq)
	secondUniqueCode, secondUniqueBody := post(uniq)

	auditsAfter, _ := env.store.ReadAudit(context.Background(), 20)

	Convey("Given an enqueue-enabled router", t, func() {
		Convey("When requests violate the §5.10 validation rules", func() {
			Convey("Then each answers 400 with a field-specific message", func() {
				So(missingTypeCode, ShouldEqual, http.StatusBadRequest)
				So(missingTypeBody, ShouldContainSubstring, "type: required")
				So(badB64Code, ShouldEqual, http.StatusBadRequest)
				So(badB64Body, ShouldContainSubstring, "payload: invalid base64")
				So(oversizeCode, ShouldEqual, http.StatusBadRequest)
				So(oversizeBody, ShouldContainSubstring, "payload: exceeds")
				So(negRetryCode, ShouldEqual, http.StatusBadRequest)
				So(negRetryBody, ShouldContainSubstring, "max_retry: must be >= 0")
				So(hugeTimeoutCode, ShouldEqual, http.StatusBadRequest)
				So(hugeTimeoutBody, ShouldContainSubstring, "timeout_seconds: must be <=")
				So(zeroUniqueCode, ShouldEqual, http.StatusBadRequest)
				So(zeroUniqueBody, ShouldContainSubstring, "unique_ttl_seconds: must be >= 1")
				So(badDeadlineCode, ShouldEqual, http.StatusBadRequest)
				So(badDeadlineBody, ShouldContainSubstring, "deadline: must be RFC3339")
				So(bothScheduleCode, ShouldEqual, http.StatusBadRequest)
				So(bothScheduleBody, ShouldContainSubstring, "mutually exclusive")
				So(farFutureCode, ShouldEqual, http.StatusBadRequest)
				So(farFutureBody, ShouldContainSubstring, "process_at: more than 1 year")
				So(unknownFieldCode, ShouldEqual, http.StatusBadRequest)
				So(unknownFieldBody, ShouldContainSubstring, "priority")
			})
			Convey("Then no rejected request wrote an audit entry", func() {
				// Only the two report:build enqueues may have audited; the
				// second one 409ed before the audit write.
				enqueueEvents := 0
				for _, a := range auditsAfter {
					if a.Event == jobs.AuditTaskEnqueued {
						enqueueEvents++
					}
				}
				So(enqueueEvents, ShouldEqual, 1)
			})
		})
		Convey("When an identical unique task is enqueued twice inside the TTL", func() {
			Convey("Then the first is created and the second answers 409", func() {
				So(firstUniqueCode, ShouldEqual, http.StatusCreated)
				So(secondUniqueCode, ShouldEqual, http.StatusConflict)
				So(secondUniqueBody, ShouldContainSubstring, "uniqueness lock")
			})
		})
	})
}
