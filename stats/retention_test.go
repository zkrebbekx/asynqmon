package stats

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Series retention and sampler-map hygiene (#52.3, #52.4), and the bounded
// group-name read (#52.6). Real Redis on DB 14 (see engine_test.go); the
// sampler clock is injected so the 24h absence is exact, never slept.
// ****************************************************************************

// srv builds one asynq server-info fixture with the given host and pid.
func srv(host string, pid int) *asynq.ServerInfo {
	return &asynq.ServerInfo{Host: host, PID: pid, Concurrency: 4}
}

func scopeIDs(servers []*asynq.ServerInfo) map[string]bool {
	return serverScopeIDs(servers)
}

func TestServerSeriesTTL(t *testing.T) {
	Convey("Given the ring TTL rule", t, func() {
		Convey("Then a server-scope rollup ring expires after 7 days, not 60", func() {
			So(rollupRing.ttlFor(seriesKeyScopeServer("pod-a:1")), ShouldEqual, 7*24*time.Hour)
			So(rollupRing.ttl, ShouldEqual, 60*24*time.Hour)
		})
		Convey("Then a server-scope hot ring keeps its own shorter TTL (the cap never lengthens one)", func() {
			So(hotRing.ttlFor(seriesKeyScopeServer("pod-a:1")), ShouldEqual, hotRing.ttl)
			So(hotRing.ttlFor(seriesKeyScopeServer("pod-a:1")), ShouldBeLessThanOrEqualTo, serverSeriesTTL)
		})
		Convey("Then fleet and queue scopes are untouched", func() {
			So(rollupRing.ttlFor(seriesKeyScopeFleet()), ShouldEqual, rollupRing.ttl)
			So(rollupRing.ttlFor(seriesKeyScopeQueue("q1")), ShouldEqual, rollupRing.ttl)
		})
	})

	Convey("Given a series key", t, func() {
		Convey("Then its scope parses back out, colons in the scope included", func() {
			scope, ok := seriesKeyScope(seriesKey(hotRing, seriesKeyScopeServer("pod-a:17"), MetricBusyWorkers))
			So(ok, ShouldBeTrue)
			So(scope, ShouldEqual, "s:pod-a:17")

			id, ok := serverScopeOf(seriesKey(rollupRing, seriesKeyScopeServer("pod-a:17"), MetricConcurrency))
			So(ok, ShouldBeTrue)
			So(id, ShouldEqual, "pod-a:17")

			_, ok = serverScopeOf(seriesKey(hotRing, seriesKeyScopeQueue("q1"), MetricPending))
			So(ok, ShouldBeFalse)
			_, ok = serverScopeOf(seriesKey(hotRing, seriesKeyScopeFleet(), MetricPending))
			So(ok, ShouldBeFalse)
		})
	})
}

// TestVanishedServerSeriesExpire covers #52.3 and #52.4 together: a pod that
// disappears has its ring keys UNLINKed after 24h, and its sampler header /
// ttlAt entries are gone after the next sweep.
func TestVanishedServerSeriesExpire(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 7, 12, 0, 7, 0, time.UTC)
	fleet := &FleetSnapshot{Pending: 3, RefreshedAt: t0}
	snaps := auxSnaps(1, t0)
	universe := auxUniverse(snaps)
	fence := newSweeperFence(rc)

	gone := srv("pod-gone", 1)
	live := srv("pod-live", 2)
	goneScope := seriesKeyScopeServer(serverScopeID(gone.Host, gone.PID))
	goneHot := seriesKey(hotRing, goneScope, MetricConcurrency)
	goneRollup := seriesKey(rollupRing, goneScope, MetricConcurrency)
	liveHot := seriesKey(hotRing, seriesKeyScopeServer(serverScopeID(live.Host, live.PID)), MetricConcurrency)

	s := newSeriesSampler(rc, 0, 0)
	tick := func(servers []*asynq.ServerInfo) seriesTick {
		return seriesTick{tier: 1, fleetSize: 1, hot: universe, servers: scopeIDs(servers)}
	}
	both := []*asynq.ServerInfo{gone, live}
	only := []*asynq.ServerInfo{live}
	sample := func(at time.Time, servers []*asynq.ServerInfo) {
		t.Helper()
		if _, _, err := s.sample(ctx, at, snaps, universe, tick(servers), fleet, servers, fence, 0); err != nil {
			t.Fatalf("sample at %s: %v", at, err)
		}
	}
	goneServerKeys := func(m map[string]bool) int {
		n := 0
		for key := range m {
			if id, ok := serverScopeOf(key); ok && id == serverScopeID(gone.Host, gone.PID) {
				n++
			}
		}
		return n
	}
	headerKeys := func() map[string]bool {
		out := make(map[string]bool, len(s.headers))
		for k := range s.headers {
			out[k] = true
		}
		return out
	}
	ttlKeys := func() map[string]bool {
		out := make(map[string]bool, len(s.ttlAt))
		for k := range s.ttlAt {
			out[k] = true
		}
		return out
	}

	// Both pods alive: the hot slots flush at +30s, the rollup slots at the
	// next hour, so every server key exists.
	sample(t0, both)
	sample(t0.Add(30*time.Second), both)
	sample(t0.Add(time.Hour), both)
	existedBefore := rc.Exists(ctx, goneHot).Val() == 1 && rc.Exists(ctx, goneRollup).Val() == 1
	headersBefore := goneServerKeys(headerKeys())

	// The pod vanishes. The first sweep after it flushes its partial slots;
	// the next one prunes the sampler maps. Both keep the keys: a restart
	// must not lose history.
	sample(t0.Add(time.Hour+30*time.Second), only)
	sample(t0.Add(time.Hour+60*time.Second), only)
	keptAfterShortAbsence := rc.Exists(ctx, goneHot).Val() == 1
	headersAfter := goneServerKeys(headerKeys())
	ttlAtAfter := goneServerKeys(ttlKeys())

	// A sweep more than 24h after the pod was last seen: its keys go.
	sample(t0.Add(26*time.Hour), only)
	goneHotAfter := rc.Exists(ctx, goneHot).Val()
	goneRollupAfter := rc.Exists(ctx, goneRollup).Val()
	liveHotAfter := rc.Exists(ctx, liveHot).Val()
	trackedAfter := len(s.serverSeen)

	Convey("Given a worker pod that vanished from Servers()", t, func() {
		Convey("Then its ring keys existed while it was alive", func() {
			So(existedBefore, ShouldBeTrue)
			So(headersBefore, ShouldBeGreaterThan, 0)
		})

		Convey("Then a short absence keeps the keys (a restart must not lose history)", func() {
			So(keptAfterShortAbsence, ShouldBeTrue)
		})

		Convey("Then the sampler prunes its headers and ttlAt entries for the vanished server (#52.4)", func() {
			So(headersAfter, ShouldEqual, 0)
			So(ttlAtAfter, ShouldEqual, 0)
		})

		Convey("Then after 24h of absence the keys are unlinked, live keys untouched (#52.3)", func() {
			So(goneHotAfter, ShouldEqual, 0)
			So(goneRollupAfter, ShouldEqual, 0)
			So(liveHotAfter, ShouldEqual, 1)
			So(trackedAfter, ShouldEqual, 1) // only the live pod is tracked
		})
	})
}

// TestGroupStallReadIsBounded covers #52.6: a queue with 1,000 groups must
// cost one SRANDMEMBER plus at most maxGroupsPerQueue ZRANGEs, never a
// full-set read.
func TestGroupStallReadIsBounded(t *testing.T) {
	rc := testRedis(t)
	_, insp := testClientInspector(t)
	ctx := context.Background()

	const groups = 1000
	pipe := rc.Pipeline()
	for i := 0; i < groups; i++ {
		g := fmt.Sprintf("g%04d", i)
		pipe.SAdd(ctx, allGroupsKey("gq"), g)
		pipe.ZAdd(ctx, groupKey("gq", g), redis.Z{Score: float64(time.Now().Add(-time.Hour).Unix()), Member: "task" + g})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("seeding groups: %v", err)
	}

	eng := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
	snaps := map[string]*QueueSnapshot{"gq": {Queue: "gq", Groups: groups}}
	obs, cmds, err := eng.readGroupStalls(ctx, snaps)

	Convey("Given a queue with 1,000 aggregation groups", t, func() {
		Convey("Then the read succeeds and observes the queue", func() {
			So(err, ShouldBeNil)
			So(obs, ShouldContainKey, "gq")
			So(obs["gq"].oldestSince.IsZero(), ShouldBeFalse)
		})

		Convey("Then at most 20 group heads are read: 1 SRANDMEMBER + 20 ZRANGE", func() {
			So(cmds, ShouldEqual, 1+maxGroupsPerQueue)
			So(cmds, ShouldBeLessThanOrEqualTo, 21)
		})
	})
}
