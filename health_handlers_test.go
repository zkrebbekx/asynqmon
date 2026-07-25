package asynqmon

import (
	"context"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/internal/leasefence"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// Handler tests for GET /api/health/roles (§3.12, phase 12) on DB 13 (the
// root fleet-handler suite's database). Fixtures seeded through the real
// asynq client; skipped when Redis does not answer PING.
// ****************************************************************************

func TestHealthRolesEndpoint(t *testing.T) {
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
	t.Cleanup(func() { client.Close(); insp.Close() })
	if _, err := client.Enqueue(asynq.NewTask("h:t", nil), asynq.Queue("hq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	engine := stats.NewEngine(stats.Config{
		RedisClient: rc,
		Inspector:   insp,
		Logf:        func(string, ...interface{}) {},
	})
	if err := engine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Simulate another replica holding the errsig indexer role.
	if tok, err := leasefence.New(rc, "errsig").Acquire(ctx, "replica-a:4242", time.Minute); err != nil || tok == 0 {
		t.Fatalf("seeding errsig lease: token=%d err=%v", tok, err)
	}

	type roleRow struct {
		Role              string `json:"role"`
		Title             string `json:"title"`
		Holder            string `json:"holder"`
		HeldByThisReplica bool   `json:"held_by_this_replica"`
		TTLMs             int64  `json:"ttl_ms"`
		Fence             int64  `json:"fence"`
	}
	var resp struct {
		Roles []roleRow `json:"roles"`
		Stats struct {
			Available              bool    `json:"available"`
			Reason                 string  `json:"reason"`
			Tier                   int     `json:"tier"`
			CommandBudget          int     `json:"command_budget"`
			HotSetSize             int     `json:"hot_set_size"`
			FullRotationSecondsEst int     `json:"full_rotation_seconds_est"`
			QueuesTotal            int     `json:"queues_total"`
			CoveragePct5m          float64 `json:"coverage_pct_5m"`
			LastSweepLocal         *struct {
				Queues     int   `json:"queues"`
				Refreshed  int   `json:"refreshed"`
				ReadCmds   int   `json:"read_cmds"`
				WriteCmds  int   `json:"write_cmds"`
				DurationMs int64 `json:"duration_ms"`
			} `json:"last_sweep_local"`
			Series *struct {
				Mode         string `json:"mode"`
				QueueCeiling int    `json:"queue_ceiling"`
			} `json:"series"`
		} `json:"stats"`
		UpdatedAt string `json:"updated_at"`
	}
	w := getJSON(t, newHealthRolesHandlerFunc(rc, engine, nil, nil), "/api/health/roles", &resp)

	var disabled struct {
		Stats struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"stats"`
	}
	wDisabled := getJSON(t, newHealthRolesHandlerFunc(rc, nil, nil, nil), "/api/health/roles", &disabled)

	Convey("Given the Settings › Health roles endpoint", t, func() {
		So(w.Code, ShouldEqual, 200)

		Convey("Then all three singleton roles are reported", func() {
			So(resp.Roles, ShouldHaveLength, 3)
			byRole := map[string]roleRow{}
			for _, r := range resp.Roles {
				byRole[r.Role] = r
			}
			So(byRole["stats"].Title, ShouldEqual, "Stats sweeper")
			So(byRole["errsig"].Title, ShouldEqual, "Error-signature indexer")
			So(byRole["hygiene"].Title, ShouldEqual, "Hygiene scheduler")

			Convey("And the seeded errsig lease shows its holder, TTL and fence", func() {
				So(byRole["errsig"].Holder, ShouldEqual, "replica-a:4242")
				So(byRole["errsig"].HeldByThisReplica, ShouldBeFalse)
				So(byRole["errsig"].TTLMs, ShouldBeGreaterThan, 0)
				So(byRole["errsig"].Fence, ShouldBeGreaterThan, 0)
			})
			Convey("And unheld roles report an empty holder", func() {
				So(byRole["hygiene"].Holder, ShouldEqual, "")
				So(byRole["hygiene"].TTLMs, ShouldEqual, 0)
			})
		})

		Convey("Then the stats block carries tier/budget/coverage and the local sweep cost", func() {
			So(resp.Stats.Available, ShouldBeTrue)
			So(resp.Stats.Tier, ShouldEqual, 1)
			So(resp.Stats.CommandBudget, ShouldEqual, stats.DefaultCommandBudget)
			So(resp.Stats.QueuesTotal, ShouldEqual, 1)
			So(resp.Stats.CoveragePct5m, ShouldEqual, 100)
			So(resp.Stats.LastSweepLocal, ShouldNotBeNil)
			So(resp.Stats.LastSweepLocal.Queues, ShouldEqual, 1)
			So(resp.Stats.LastSweepLocal.ReadCmds, ShouldBeGreaterThan, 0)
			So(resp.Stats.Series, ShouldNotBeNil)
			So(resp.Stats.Series.Mode, ShouldEqual, "full")
			So(resp.Stats.Series.QueueCeiling, ShouldBeGreaterThan, stats.Tier2MaxQueues)
		})

		Convey("Then a stats-disabled replica reports honest unavailability, roles intact", func() {
			So(wDisabled.Code, ShouldEqual, 200)
			So(disabled.Stats.Available, ShouldBeFalse)
			So(disabled.Stats.Reason, ShouldContainSubstring, "disabled")
		})
	})
}
