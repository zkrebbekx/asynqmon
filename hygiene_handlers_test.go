package asynqmon

import (
	"context"
	"net/http"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/hygiene"
	"github.com/hibiken/asynqmon/jobs"
)

// ****************************************************************************
// HTTP-surface tests for the §3.10 hygiene endpoints — against a real Redis
// on 127.0.0.1:6379 DB 7. DB 7 is reserved for the hygiene suites (this file
// + hygiene_integration_test.go; the other root-package suites own DBs 8-13)
// and is the ONLY database these tests flush. Skipped when Redis does not
// answer PING.
//
// Convey discipline (jobs_handlers_test.go precedent): flows that hit Redis
// run imperatively BEFORE the Convey tree; the tree only reads captured
// results.
//
// The JSON field names asserted here are FROZEN frontend contracts
// (ui/src/api-hygiene.ts).
// ****************************************************************************

const (
	hygieneTestRedisAddr = "127.0.0.1:6379"
	hygieneTestRedisDB   = 7
)

type hygieneTestEnv struct {
	rc   *redis.Client
	insp *asynq.Inspector
}

// newHygieneTestEnv flushes DB 7 and returns clients wired to it.
func newHygieneTestEnv(t *testing.T) *hygieneTestEnv {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: hygieneTestRedisAddr, DB: hygieneTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", hygieneTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", hygieneTestRedisDB, err)
	}
	insp := asynq.NewInspector(asynq.RedisClientOpt{Addr: hygieneTestRedisAddr, DB: hygieneTestRedisDB})
	t.Cleanup(func() {
		insp.Close()
		rc.Close()
	})
	return &hygieneTestEnv{rc: rc, insp: insp}
}

// newRouter builds the real router. statsEngine stays nil, so the route
// block's fallback hygiene engine has no snapshot source — exactly the
// "stats engine disabled" deployment.
func (e *hygieneTestEnv) newRouter(opts Options) http.Handler {
	return muxRouter(opts, e.rc, e.insp, nil, nil)
}

// hygieneListBody mirrors listHygieneResponse for decoding.
type hygieneListBody struct {
	Reports []struct {
		Kind            string           `json:"kind"`
		Title           string           `json:"title"`
		Enabled         bool             `json:"enabled"`
		IntervalSeconds int64            `json:"interval_seconds"`
		LastGeneratedAt string           `json:"last_generated_at"`
		Summary         map[string]int64 `json:"summary"`
	} `json:"reports"`
}

func TestHygieneEndpoints(t *testing.T) {
	env := newHygieneTestEnv(t)
	h := env.newRouter(Options{})

	// -- GET /api/hygiene on a virgin store: defaults, nothing generated. --
	var list hygieneListBody
	listRec := doJSON(t, h, "GET", "/api/hygiene", nil, &list)

	// -- Kind validation on every route. --
	badGet := doJSON(t, h, "GET", "/api/hygiene/nonsense", nil, nil)
	badRun := doJSON(t, h, "POST", "/api/hygiene/nonsense/run", nil, nil)
	badPut := doJSON(t, h, "PUT", "/api/hygiene/nonsense/config",
		map[string]interface{}{"enabled": true, "interval_seconds": 3600}, nil)

	// -- GET a never-generated report. --
	notFound := doJSON(t, h, "GET", "/api/hygiene/inventory", nil, nil)

	// -- Run without a stats snapshot source: snapshot-dependent kinds are an
	// honest 503; scheduler-health works (Servers()/SchedulerEntries only). --
	runNoStats := doJSON(t, h, "POST", "/api/hygiene/inventory/run", nil, nil)
	var shReport hygiene.Report
	runSH := doJSON(t, h, "POST", "/api/hygiene/scheduler-health/run", nil, &shReport)
	var shFull hygiene.Report
	getSH := doJSON(t, h, "GET", "/api/hygiene/scheduler-health", nil, &shFull)

	// -- PUT config: validation, persistence, audit. --
	var putResp map[string]interface{}
	putOK := doJSON(t, h, "PUT", "/api/hygiene/inventory/config",
		map[string]interface{}{"enabled": true, "interval_seconds": 3600}, &putResp)
	putBadInterval := doJSON(t, h, "PUT", "/api/hygiene/inventory/config",
		map[string]interface{}{"enabled": true, "interval_seconds": 30}, nil)
	var listAfter hygieneListBody
	doJSON(t, h, "GET", "/api/hygiene", nil, &listAfter)

	audit, auditErr := jobs.NewStore(env.rc).ReadAudit(context.Background(), 50)

	Convey("Given the hygiene HTTP surface on a fresh store", t, func() {
		Convey("Then GET /api/hygiene lists all four kinds, disabled at the mockup default cadences", func() {
			So(listRec.Code, ShouldEqual, http.StatusOK)
			So(list.Reports, ShouldHaveLength, 4)
			byKind := map[string]int64{}
			for _, r := range list.Reports {
				So(r.Enabled, ShouldBeFalse)
				So(r.LastGeneratedAt, ShouldEqual, "")
				So(r.Summary, ShouldBeNil)
				So(r.Title, ShouldNotBeBlank)
				byKind[r.Kind] = r.IntervalSeconds
			}
			So(byKind["inventory"], ShouldEqual, 7*24*3600)
			So(byKind["storage"], ShouldEqual, 30*24*3600)
			So(byKind["dead-letter"], ShouldEqual, 7*24*3600)
			So(byKind["scheduler-health"], ShouldEqual, 24*3600)
		})

		Convey("Then unknown kinds are 400 on GET, run and config", func() {
			So(badGet.Code, ShouldEqual, http.StatusBadRequest)
			So(badRun.Code, ShouldEqual, http.StatusBadRequest)
			So(badPut.Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("Then a never-generated report is a 404 with guidance, not fabricated zeros", func() {
			So(notFound.Code, ShouldEqual, http.StatusNotFound)
			So(notFound.Body.String(), ShouldContainSubstring, "Run now")
		})

		Convey("Then snapshot-dependent kinds honestly 503 without the stats engine", func() {
			So(runNoStats.Code, ShouldEqual, http.StatusServiceUnavailable)
			So(runNoStats.Body.String(), ShouldContainSubstring, "stats")
		})

		Convey("Then scheduler-health runs without stats, returns and persists the report", func() {
			So(runSH.Code, ShouldEqual, http.StatusOK)
			So(shReport.Kind, ShouldEqual, "scheduler-health")
			So(shReport.SchedulerHealth, ShouldNotBeNil)
			So(getSH.Code, ShouldEqual, http.StatusOK)
			So(shFull.GeneratedAt.IsZero(), ShouldBeFalse)
		})

		Convey("Then PUT config validates the interval, persists, and surfaces on the list", func() {
			So(putOK.Code, ShouldEqual, http.StatusOK)
			So(putResp["kind"], ShouldEqual, "inventory")
			So(putBadInterval.Code, ShouldEqual, http.StatusBadRequest)
			So(putBadInterval.Body.String(), ShouldContainSubstring, "interval_seconds")
			for _, r := range listAfter.Reports {
				if r.Kind == "inventory" {
					So(r.Enabled, ShouldBeTrue)
					So(r.IntervalSeconds, ShouldEqual, 3600)
				}
			}
		})

		Convey("Then run triggers (including the failed one) and the config change are audit-logged with the actor", func() {
			So(auditErr, ShouldBeNil)
			runVerbs := map[string]int{}
			var cfgs int
			for _, e := range audit {
				switch e.Event {
				case jobs.AuditHygieneRun:
					runVerbs[e.Verb]++
					So(e.Actor, ShouldStartWith, "anonymous@")
				case jobs.AuditHygieneConfig:
					cfgs++
					So(e.Verb, ShouldEqual, "inventory")
					So(e.Reason, ShouldContainSubstring, "enabled")
				}
			}
			// The 503'd inventory run is logged too: triggers are recorded
			// BEFORE generation so induced load stays attributable.
			So(runVerbs["inventory"], ShouldEqual, 1)
			So(runVerbs["scheduler-health"], ShouldEqual, 1)
			So(cfgs, ShouldEqual, 1)
		})
	})
}

func TestHygieneReadOnlyMode(t *testing.T) {
	env := newHygieneTestEnv(t)
	h := env.newRouter(Options{ReadOnly: true})

	// Run-now must survive read-only mode (reports are reads; the §3.10
	// endpoint spec documents this) while the config PUT is blocked.
	var rep hygiene.Report
	runRec := doJSON(t, h, "POST", "/api/hygiene/scheduler-health/run", nil, &rep)
	putRec := doJSON(t, h, "PUT", "/api/hygiene/scheduler-health/config",
		map[string]interface{}{"enabled": true, "interval_seconds": 3600}, nil)
	listRec := doJSON(t, h, "GET", "/api/hygiene", nil, nil)

	audit, auditErr := jobs.NewStore(env.rc).ReadAudit(context.Background(), 10)

	Convey("Given the hygiene surface in read-only mode", t, func() {
		Convey("Then Run-now still works and is audit-logged", func() {
			So(runRec.Code, ShouldEqual, http.StatusOK)
			So(rep.Kind, ShouldEqual, "scheduler-health")
			So(auditErr, ShouldBeNil)
			var runs int
			for _, e := range audit {
				if e.Event == jobs.AuditHygieneRun {
					runs++
				}
			}
			So(runs, ShouldEqual, 1)
		})

		Convey("Then the schedule config PUT is blocked by the read-only filter", func() {
			So(putRec.Code, ShouldEqual, http.StatusMethodNotAllowed)
		})

		Convey("Then reads keep working", func() {
			So(listRec.Code, ShouldEqual, http.StatusOK)
		})
	})
}
