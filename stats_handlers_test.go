package asynqmon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Handler tests for the /api/fleet endpoints, against a real Redis on
// 127.0.0.1:6379 DB 13. (The stats package's own integration tests own DB 14;
// separate DBs keep parallel per-package test runs from flushing each other.)
// Fixtures are seeded through the real asynq client so key layouts are
// authentic. Skipped when Redis does not answer PING.
// ****************************************************************************

const (
	statsTestRedisAddr = "127.0.0.1:6379"
	statsTestRedisDB   = 13
)

// newStatsTestEngine flushes DB 13, seeds three queues through the real
// asynq client (alpha: 1 pending, beta: 3 pending, gamma: 5 pending +
// paused), runs one sweep, and returns the engine.
func newStatsTestEngine(t *testing.T) *stats.Engine {
	t.Helper()
	ctx := context.Background()

	rc := redis.NewClient(&redis.Options{Addr: statsTestRedisAddr, DB: statsTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", statsTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })

	opt := asynq.RedisClientOpt{Addr: statsTestRedisAddr, DB: statsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() {
		client.Close()
		insp.Close()
	})

	seed := map[string]int{"alpha": 1, "beta": 3, "gamma": 5}
	for qname, n := range seed {
		for i := 0; i < n; i++ {
			if _, err := client.Enqueue(asynq.NewTask("seed:task", nil), asynq.Queue(qname)); err != nil {
				t.Fatalf("enqueue into %s: %v", qname, err)
			}
		}
	}
	if err := insp.PauseQueue("gamma"); err != nil {
		t.Fatalf("pause gamma: %v", err)
	}

	engine := stats.NewEngine(stats.Config{
		RedisClient: rc,
		Inspector:   insp,
		Logf:        func(string, ...interface{}) {},
	})
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return engine
}

func getJSON(t *testing.T, h http.HandlerFunc, url string, out interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	h(w, req)
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding %s response: %v\nbody: %s", url, err, w.Body.String())
		}
	}
	return w
}

func TestFleetOverviewHandler(t *testing.T) {
	engine := newStatsTestEngine(t)
	handler := newFleetOverviewHandlerFunc(engine)

	Convey("Given a swept three-queue fleet", t, func() {
		Convey("When GET /api/fleet/overview is served", func() {
			var resp fleetOverviewResponse
			w := getJSON(t, handler, "/api/fleet/overview", &resp)

			Convey("Then it returns the fleet aggregates", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(resp.Fleet.QueuesTotal, ShouldEqual, 3)
				So(resp.Fleet.Pending, ShouldEqual, 9)
				So(resp.Fleet.PausedQueues, ShouldEqual, 1)
				So(resp.Fleet.ZeroConsumerQueues, ShouldEqual, 3) // no servers running
				So(resp.Fleet.Servers, ShouldEqual, 0)
				So(resp.Fleet.OrphanCandidates, ShouldEqual, 0)
			})
			Convey("Then it carries the Redis memory gauge for the Redis tile", func() {
				So(resp.Fleet.RedisMemoryUsedBytes, ShouldBeGreaterThan, 0)
				So(resp.Fleet.RedisMemoryMaxBytes, ShouldBeGreaterThanOrEqualTo, 0)
			})
			Convey("Then it carries the honest coverage stamp", func() {
				So(resp.Coverage.Tier, ShouldEqual, 1)
				So(resp.Coverage.QueuesTotal, ShouldEqual, 3)
				So(resp.Coverage.RefreshedPct5m, ShouldEqual, 100)
				So(resp.Coverage.UpdatedAt, ShouldNotBeEmpty)
				So(resp.Coverage.Source, ShouldEqual, stats.SourceLocal)
			})
		})

		Convey("When the stats engine is disabled (nil)", func() {
			w := getJSON(t, newFleetOverviewHandlerFunc(nil), "/api/fleet/overview", nil)
			Convey("Then it responds 503 with a JSON error", func() {
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(w.Body.String(), ShouldContainSubstring, "disabled")
			})
		})
	})
}

func TestFleetQueuesHandler(t *testing.T) {
	engine := newStatsTestEngine(t)
	handler := newFleetQueuesHandlerFunc(engine, nil)

	queueNames := func(resp fleetQueuesResponse) []string {
		names := make([]string, 0, len(resp.Queues))
		for _, q := range resp.Queues {
			names = append(names, q.Queue)
		}
		return names
	}

	Convey("Given a swept fleet of alpha(1)/beta(3)/gamma(5, paused) pending", t, func() {
		Convey("When sorted by pending descending", func() {
			var resp fleetQueuesResponse
			w := getJSON(t, handler, "/api/fleet/queues?sort=pending&dir=desc", &resp)
			Convey("Then the deepest queue is first and rows carry refreshed_at", func() {
				So(w.Code, ShouldEqual, http.StatusOK)
				So(queueNames(resp), ShouldResemble, []string{"gamma", "beta", "alpha"})
				So(resp.TotalMatched, ShouldEqual, 3)
				So(resp.NextCursor, ShouldBeNil)
				for _, q := range resp.Queues {
					So(q.RefreshedAt, ShouldNotBeEmpty)
				}
				So(resp.Coverage.Tier, ShouldEqual, 1)
			})
			Convey("Then the paused row exposes paused state and pause time", func() {
				So(resp.Queues[0].Paused, ShouldBeTrue)
				So(resp.Queues[0].PausedSince, ShouldNotBeEmpty)
				So(resp.Queues[1].Paused, ShouldBeFalse)
				So(resp.Queues[1].PausedSince, ShouldBeEmpty)
			})
			Convey("Then latency_ms mirrors oldest_pending_age_ms (same metric, both columns)", func() {
				So(resp.Queues[0].LatencyMs, ShouldEqual, resp.Queues[0].OldestPendingAgeMs)
				So(resp.Queues[0].OldestPendingAgeMs, ShouldBeGreaterThan, 0)
			})
		})

		Convey("When paginating with limit=2", func() {
			var page1 fleetQueuesResponse
			getJSON(t, handler, "/api/fleet/queues?sort=pending&dir=desc&limit=2", &page1)
			Convey("Then page one returns a cursor to page two", func() {
				So(queueNames(page1), ShouldResemble, []string{"gamma", "beta"})
				So(page1.TotalMatched, ShouldEqual, 3)
				So(page1.NextCursor, ShouldNotBeNil)
				So(*page1.NextCursor, ShouldEqual, 2)

				var page2 fleetQueuesResponse
				getJSON(t, handler, "/api/fleet/queues?sort=pending&dir=desc&limit=2&cursor=2", &page2)
				Convey("And page two returns the remainder with no cursor", func() {
					So(queueNames(page2), ShouldResemble, []string{"alpha"})
					So(page2.NextCursor, ShouldBeNil)
				})
			})
		})

		Convey("When filtering", func() {
			Convey("Then a numeric clause narrows the set", func() {
				var resp fleetQueuesResponse
				getJSON(t, handler, "/api/fleet/queues?f=pending%3E2&sort=pending&dir=asc", &resp)
				So(queueNames(resp), ShouldResemble, []string{"beta", "gamma"})
				So(resp.TotalMatched, ShouldEqual, 2)
			})
			Convey("Then paused=true matches only the paused queue", func() {
				var resp fleetQueuesResponse
				getJSON(t, handler, "/api/fleet/queues?f=paused%3Dtrue", &resp)
				So(queueNames(resp), ShouldResemble, []string{"gamma"})
			})
			Convey("Then a name substring matches", func() {
				var resp fleetQueuesResponse
				getJSON(t, handler, "/api/fleet/queues?f=alph", &resp)
				So(queueNames(resp), ShouldResemble, []string{"alpha"})
			})
			Convey("Then unparsable tokens are surfaced in filter_ignored, not silently dropped", func() {
				var resp fleetQueuesResponse
				getJSON(t, handler, "/api/fleet/queues?f=bogus%3D1+pending%3E2", &resp)
				So(resp.FilterIgnored, ShouldResemble, []string{"bogus=1"})
				So(resp.TotalMatched, ShouldEqual, 2)
			})
		})

		Convey("When parameters are invalid", func() {
			Convey("Then an unknown sort column is a 400", func() {
				w := getJSON(t, handler, "/api/fleet/queues?sort=memory", nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then a bad dir is a 400", func() {
				w := getJSON(t, handler, "/api/fleet/queues?dir=sideways", nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then a negative cursor is a 400", func() {
				w := getJSON(t, handler, "/api/fleet/queues?cursor=-1", nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then an out-of-range limit is a 400", func() {
				w := getJSON(t, handler, "/api/fleet/queues?limit=9999", nil)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
			Convey("Then a cursor past the end returns an empty page, not an error", func() {
				var resp fleetQueuesResponse
				w := getJSON(t, handler, "/api/fleet/queues?cursor=50", &resp)
				So(w.Code, ShouldEqual, http.StatusOK)
				So(resp.Queues, ShouldBeEmpty)
				So(resp.TotalMatched, ShouldEqual, 3)
			})
		})

		Convey("When the stats engine is disabled (nil)", func() {
			w := getJSON(t, newFleetQueuesHandlerFunc(nil, nil), "/api/fleet/queues", nil)
			Convey("Then it responds 503 with a JSON error", func() {
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
			})
		})
	})
}
