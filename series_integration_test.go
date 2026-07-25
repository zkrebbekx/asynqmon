package asynqmon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/jobs"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Integration tests for the §5.8 series substrate + §5.14 deploy markers
// against a real Redis on 127.0.0.1:6379 DB 8.
//
// DB 8 is reserved for THIS file (other root-package suites use DB 13, the
// stats package uses DB 14) and is the ONLY database these tests flush.
//
// The sweep clock is injected (stats.Config.Now) so ring-slot boundaries are
// crossed deterministically — no wall-clock sleeps, no flakes. Fixtures are
// seeded through the real asynq client; the two documented exceptions are
// daily counters (hand-INCR'd: only a full asynq server produces them, and
// past days cannot be produced at all) whose key format is tripwire-verified
// by the stats package against asynq v0.24.1.
// ****************************************************************************

const (
	seriesTestRedisAddr = "127.0.0.1:6379"
	seriesTestRedisDB   = 8
)

func seriesTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: seriesTestRedisAddr, DB: seriesTestRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", seriesTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", seriesTestRedisDB, err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

func seriesTestClientInspector(t *testing.T) (*asynq.Client, *asynq.Inspector) {
	t.Helper()
	opt := asynq.RedisClientOpt{Addr: seriesTestRedisAddr, DB: seriesTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
	})
	return client, insp
}

// testClock is the injected sweep clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// seriesTestBase picks a synthetic sweep start: aligned on a 30s hot-slot
// boundary, at least 2m in the past (so the wall-clock read path's window
// covers it) and never within the first 90s of an hour (so no rollup-slot
// boundary is crossed mid-test). Same UTC day as the wall clock except in
// the first test-hour after midnight — all daily-counter fixtures therefore
// derive their dates from THIS time, never from time.Now().
func seriesTestBase() time.Time {
	t0 := time.Now().UTC().Truncate(time.Hour).Add(90 * time.Second)
	if t0.After(time.Now().Add(-2 * time.Minute)) {
		t0 = t0.Add(-time.Hour)
	}
	return t0
}

// rawFailedKey mirrors asynq's daily failed-counter key (tripwire-verified
// in stats/engine_test.go TestMirroredKeyFormats).
func rawFailedKey(q string, day time.Time) string {
	return "asynq:{" + q + "}:failed:" + day.UTC().Format("2006-01-02")
}

func rawProcessedKey(q string, day time.Time) string {
	return "asynq:{" + q + "}:processed:" + day.UTC().Format("2006-01-02")
}

// TestSeriesSweepPipeline drives sweep → SETRANGE ring → read API end to
// end: byte-level ring assertion once, then the same data through
// stats.ReadSeries and the HTTP handlers.
func TestSeriesSweepPipeline(t *testing.T) {
	rc := seriesTestRedis(t)
	client, insp := seriesTestClientInspector(t)
	ctx := context.Background()

	// q1: 3 pending tasks, seeded through the real asynq client.
	for i := 0; i < 3; i++ {
		if _, err := client.Enqueue(asynq.NewTask("t:pending", nil), asynq.Queue("q1")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	clock := &testClock{t: seriesTestBase()}
	t0 := clock.Now()
	engine := stats.NewEngine(stats.Config{
		RedisClient: rc,
		Inspector:   insp,
		Now:         clock.Now,
		Logf:        func(string, ...interface{}) {},
	})

	// Sweep 1 at t0 (hot slot A): accumulates only — a slot is flushed when
	// the clock first crosses OUT of it, so nothing is in Redis yet.
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Counter movement between sweeps: +10 processed / +4 failed on q1
	// (documented hand-built fixture — see file header).
	today := t0.UTC()
	if err := rc.Set(ctx, rawProcessedKey("q1", today), 10, 0).Err(); err != nil {
		t.Fatalf("seed processed: %v", err)
	}
	if err := rc.Set(ctx, rawFailedKey("q1", today), 4, 0).Err(); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	// Sweep 2 at t0+30s (slot B): flushes slot A for the gauge series; the
	// first delta sample (10, 4) starts accumulating into slot B.
	clock.Advance(30 * time.Second)
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	// Sweep 3 at t0+60s (slot C): flushes slot B (gauges + the deltas).
	clock.Advance(30 * time.Second)
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}

	hotPeriodA := t0.Unix() / 30
	slotFor := func(p int64) int { return int(p % 360) }

	Convey("Given three deterministic sweeps across two hot-slot boundaries", t, func() {
		Convey("Then the fleet pending hot ring matches the §5.8 encoding byte for byte", func() {
			raw, err := rc.Get(ctx, "asynqmon:series:h:fleet:pending").Bytes()
			So(err, ShouldBeNil)

			// Expected buffer, built independently of the writer: 12-byte
			// header 'A','S',ver,0, first=A, last=B (uint32 BE), then 360
			// uint32 slots — sentinel everywhere except A=3 and B=3.
			expected := make([]byte, 12+360*4)
			expected[0], expected[1], expected[2], expected[3] = 'A', 'S', 0x01, 0x00
			binary.BigEndian.PutUint32(expected[4:8], uint32(hotPeriodA))
			binary.BigEndian.PutUint32(expected[8:12], uint32(hotPeriodA+1))
			for i := 12; i < len(expected); i++ {
				expected[i] = 0xFF
			}
			binary.BigEndian.PutUint32(expected[12+slotFor(hotPeriodA)*4:], 3)
			binary.BigEndian.PutUint32(expected[12+slotFor(hotPeriodA+1)*4:], 3)

			So(len(raw), ShouldEqual, len(expected))
			So(string(raw), ShouldEqual, string(expected))
		})

		Convey("Then the ring strings carry their safety-net TTLs", func() {
			So(rc.TTL(ctx, "asynqmon:series:h:fleet:pending").Val(), ShouldBeGreaterThan, time.Hour)
			So(rc.TTL(ctx, "asynqmon:series:h:q:q1:failed_delta").Val(), ShouldBeGreaterThan, time.Hour)
		})

		Convey("Then stats.ReadSeries serves the flushed slots and nulls everything else", func() {
			scope, err := stats.ParseSeriesScope("queue:q1")
			So(err, ShouldBeNil)
			s, err := stats.ReadSeries(ctx, rc, stats.SeriesSpec{
				Scope: scope, Metric: "pending", Window: "3h",
			}, clock.Now())
			So(err, ShouldBeNil)
			So(s.Period, ShouldEqual, 30)
			So(s.Points, ShouldHaveLength, 360)
			byT := map[int64]*int64{}
			for _, p := range s.Points {
				byT[p.T] = p.V
			}
			So(*byT[hotPeriodA*30], ShouldEqual, 3)
			So(*byT[(hotPeriodA+1)*30], ShouldEqual, 3)
			So(byT[(hotPeriodA+2)*30], ShouldBeNil) // slot C: still accumulating
			So(byT[(hotPeriodA-1)*30], ShouldBeNil) // before sampling began
		})

		Convey("Then delta series honor 'first sweep = no-sample, never zero'", func() {
			scope, _ := stats.ParseSeriesScope("queue:q1")
			s, err := stats.ReadSeries(ctx, rc, stats.SeriesSpec{
				Scope: scope, Metric: "failed_delta", Window: "3h",
			}, clock.Now())
			So(err, ShouldBeNil)
			byT := map[int64]*int64{}
			for _, p := range s.Points {
				byT[p.T] = p.V
			}
			// Slot A: the sweep ran there, but with no previous counter there
			// was no delta — no-sample, NOT zero.
			So(byT[hotPeriodA*30], ShouldBeNil)
			// Slot B: the movement observed between sweeps 1→2.
			So(*byT[(hotPeriodA+1)*30], ShouldEqual, 4)
		})

		Convey("Then gauges emit real zeros (distinct from no-sample)", func() {
			s, err := stats.ReadSeries(ctx, rc, stats.SeriesSpec{
				Scope: stats.SeriesScope{Kind: "fleet"}, Metric: "busy_workers", Window: "3h",
			}, clock.Now())
			So(err, ShouldBeNil)
			byT := map[int64]*int64{}
			for _, p := range s.Points {
				byT[p.T] = p.V
			}
			v := byT[hotPeriodA*30]
			So(v, ShouldNotBeNil) // no servers running: busy 0 is a SAMPLE
			So(*v, ShouldEqual, 0)
		})

		Convey("Then the HTTP read API serves the same numbers (production reader, wall clock)", func() {
			h := newGetSeriesHandlerFunc(newStatsSeriesReader(rc))
			req := httptest.NewRequest("GET", "/api/series?scope=fleet&metric=pending&window=3h", nil)
			rr := httptest.NewRecorder()
			h(rr, req)
			So(rr.Code, ShouldEqual, http.StatusOK)
			var body struct {
				Period        int64 `json:"period"`
				LearningUntil string `json:"learning_until"`
				Points        []struct {
					T int64  `json:"t"`
					V *int64 `json:"v"`
				} `json:"points"`
			}
			So(json.Unmarshal(rr.Body.Bytes(), &body), ShouldBeNil)
			So(body.Period, ShouldEqual, 30)
			// Sampling began minutes ago → the 24h learning stamp is present.
			So(body.LearningUntil, ShouldNotBeEmpty)
			byT := map[int64]*int64{}
			for _, p := range body.Points {
				byT[p.T] = p.V
			}
			So(*byT[hotPeriodA*30], ShouldEqual, 3)
			So(byT[(hotPeriodA+2)*30], ShouldBeNil)
		})

		Convey("Then the batch endpoint answers multiple specs in one round trip", func() {
			h := newGetSeriesBatchHandlerFunc(newStatsSeriesReader(rc))
			specs := `[{"scope":"fleet","metric":"pending","window":"3h","points":60},` +
				`{"scope":"queue:q1","metric":"failed_delta","window":"24h"}]`
			req := httptest.NewRequest("GET", "/api/series/batch?specs="+strings.ReplaceAll(specs, " ", ""), nil)
			rr := httptest.NewRecorder()
			h(rr, req)
			So(rr.Code, ShouldEqual, http.StatusOK)
			var body struct {
				Series []struct {
					Scope  string `json:"scope"`
					Window string `json:"window"`
					Points []struct {
						T int64  `json:"t"`
						V *int64 `json:"v"`
					} `json:"points"`
				} `json:"series"`
			}
			So(json.Unmarshal(rr.Body.Bytes(), &body), ShouldBeNil)
			So(body.Series, ShouldHaveLength, 2)
			So(body.Series[0].Scope, ShouldEqual, "fleet")
			So(body.Series[0].Points, ShouldHaveLength, 60)
			So(body.Series[1].Scope, ShouldEqual, "queue:q1")
			So(body.Series[1].Points, ShouldHaveLength, 24) // merged hourly day view
			// The 24h merge folds the un-flushed recent hour from hot slots:
			// the failure delta of 4 must appear in its hour bucket.
			hourT := (t0.Unix() / 3600) * 3600
			var hourV *int64
			for _, p := range body.Series[1].Points {
				if p.T == hourT {
					hourV = p.V
				}
			}
			So(hourV, ShouldNotBeNil)
			So(*hourV, ShouldEqual, 4)
		})
	})
}

// TestSeriesDetectorIntegration exercises FAIL_SPIKE against real 7-day
// daily counters plus the learning census over a fresh substrate.
func TestSeriesDetectorIntegration(t *testing.T) {
	rc := seriesTestRedis(t)
	client, insp := seriesTestClientInspector(t)
	ctx := context.Background()

	clock := &testClock{t: seriesTestBase()}
	t0 := clock.Now()

	// qspike exists (one real pending task) and carries hand-seeded daily
	// counters: ~10 failures/day for the last 7 days, 800 today — an
	// unambiguous spike at any hour of day under ratio 4x / min 100.
	if _, err := client.Enqueue(asynq.NewTask("t:x", nil), asynq.Queue("qspike")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 1; i <= 7; i++ {
		if err := rc.Set(ctx, rawFailedKey("qspike", t0.UTC().AddDate(0, 0, -i)), 10, 0).Err(); err != nil {
			t.Fatalf("seed history day -%d: %v", i, err)
		}
	}
	if err := rc.Set(ctx, rawFailedKey("qspike", t0.UTC()), 800, 0).Err(); err != nil {
		t.Fatalf("seed today: %v", err)
	}
	if err := rc.Set(ctx, rawProcessedKey("qspike", t0.UTC()), 900, 0).Err(); err != nil {
		t.Fatalf("seed processed: %v", err)
	}

	engine := stats.NewEngine(stats.Config{
		RedisClient: rc,
		Inspector:   insp,
		Now:         clock.Now,
		Logf:        func(string, ...interface{}) {},
	})
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rep, err := engine.ReadAttention(ctx)
	if err != nil {
		t.Fatalf("read attention: %v", err)
	}

	Convey("Given a queue failing 80x its 7-day baseline on a fresh series substrate", t, func() {
		Convey("Then FAIL_SPIKE fires on day one (daily counters pre-exist) with its series ref", func() {
			var f *stats.AttentionFinding
			for i := range rep.Findings {
				if rep.Findings[i].Detector == stats.DetectorFailSpike {
					f = &rep.Findings[i]
				}
			}
			So(f, ShouldNotBeNil)
			So(f.Queue, ShouldEqual, "qspike")
			So(f.Value, ShouldEqual, 800)
			So(f.Sentence, ShouldContainSubstring, "7-day same-hour baseline")
			So(f.SeriesScope, ShouldEqual, "queue:qspike")
			So(f.SeriesMetric, ShouldEqual, "failed_delta")
		})

		Convey("Then the census reports 10 live + 2 learning with exact anchored go-live times", func() {
			So(rep.DetectorsLive, ShouldEqual, 10)
			So(rep.DetectorsLearning, ShouldEqual, 2)
			So(rep.Learning, ShouldHaveLength, 2)
			byDetector := map[string]string{}
			for _, l := range rep.Learning {
				byDetector[l.Detector] = l.Until
			}
			// The anchor is the asynqmon:series:since key written by this
			// sweep with the injected clock — untils are exact instants, not
			// "roughly now plus a day". (Compared as instants: the report
			// formats in the server's zone.)
			depthUntil, err := time.Parse(time.RFC3339, byDetector[stats.DetectorDepthAnomaly])
			So(err, ShouldBeNil)
			So(depthUntil.Unix(), ShouldEqual, t0.Add(24*time.Hour).Unix())
			consUntil, err := time.Parse(time.RFC3339, byDetector[stats.DetectorConsumersDropped])
			So(err, ShouldBeNil)
			So(consUntil.Unix(), ShouldEqual, t0.Add(30*time.Minute).Unix())
		})
	})
}

// TestMarkersIntegration drives the §5.14 endpoints against the real store
// and audit stream.
func TestMarkersIntegration(t *testing.T) {
	rc := seriesTestRedis(t)
	ctx := context.Background()

	store := newRedisMarkerStore(rc)
	audit := jobs.NewStore(rc)
	create := newCreateMarkerHandlerFunc(store, audit)
	list := newListMarkersHandlerFunc(store)

	post := func(label, actor string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/markers", strings.NewReader(`{"label":`+strconv.Quote(label)+`}`))
		req = req.WithContext(context.WithValue(req.Context(), actorCtxKey{}, actorInfo{Name: actor, Attributed: true}))
		rr := httptest.NewRecorder()
		create(rr, req)
		return rr
	}

	rr1 := post("deploy v2.4.1", "zac@example.com")
	// Millisecond-precision scores order same-second markers; keep the two
	// posts in distinct milliseconds so the ascending assertion is exact.
	time.Sleep(5 * time.Millisecond)
	rr2 := post("rollback v2.4.1", "ci@deploys")

	Convey("Given two markers posted through the handler", t, func() {
		So(rr1.Code, ShouldEqual, http.StatusCreated)
		So(rr2.Code, ShouldEqual, http.StatusCreated)

		Convey("Then they live in the asynqmon:markers zset scored by time", func() {
			So(rc.ZCard(ctx, "asynqmon:markers").Val(), ShouldEqual, 2)
		})

		Convey("Then GET /api/markers returns them ascending with actors", func() {
			req := httptest.NewRequest("GET", "/api/markers?window=24h", nil)
			rr := httptest.NewRecorder()
			list(rr, req)
			So(rr.Code, ShouldEqual, http.StatusOK)
			var body listMarkersResponse
			So(json.Unmarshal(rr.Body.Bytes(), &body), ShouldBeNil)
			So(body.Markers, ShouldHaveLength, 2)
			So(body.Markers[0].Label, ShouldEqual, "deploy v2.4.1")
			So(body.Markers[0].Actor, ShouldEqual, "zac@example.com")
			So(body.Markers[1].Label, ShouldEqual, "rollback v2.4.1")
		})

		Convey("Then every creation is audit-logged with the resolved actor (§5.11)", func() {
			entries, err := audit.ReadAudit(ctx, 10)
			So(err, ShouldBeNil)
			So(entries, ShouldHaveLength, 2)
			// Newest first in the stream.
			So(entries[0].Event, ShouldEqual, "marker_created")
			So(entries[0].Actor, ShouldEqual, "ci@deploys")
			So(entries[0].Reason, ShouldEqual, "rollback v2.4.1")
			So(entries[1].Actor, ShouldEqual, "zac@example.com")
		})
	})
}
