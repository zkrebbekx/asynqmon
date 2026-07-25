package hygiene

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Fencing test for the hygiene scheduler role (§5.13, phase 12) against a
// real Redis on 127.0.0.1:6379, DB 5 (unused elsewhere: errsig fence uses 6,
// root suites 7-13, stats 14, leasefence 15). Skipped when Redis does not
// answer PING.
// ****************************************************************************

const (
	fenceTestRedisAddr = "127.0.0.1:6379"
	fenceTestRedisDB   = 5
)

func fenceTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", fenceTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

// TestSchedulerFencing: a scheduled report persist under a superseded token
// is rejected; the operator-triggered Run (token 0) still writes.
func TestSchedulerFencing(t *testing.T) {
	rc := fenceTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB}
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { insp.Close() })

	eng := NewEngine(Config{
		RedisClient: rc,
		Inspector:   insp,
		Logf:        func(string, ...interface{}) {},
	})

	// Claim as this scheduler (token1), then a newer replica supersedes it.
	token1, err := eng.fence.Acquire(ctx, eng.InstanceID(), time.Minute)
	if err != nil || token1 == 0 {
		t.Fatalf("acquire: token=%d err=%v", token1, err)
	}
	atomic.StoreInt64(&eng.token, token1)
	atomic.StoreInt32(&eng.holding, 1)
	if err := eng.fence.Release(ctx, eng.InstanceID()); err != nil {
		t.Fatalf("release: %v", err)
	}
	token2, err := eng.fence.Acquire(ctx, "newer-replica", time.Minute)
	if err != nil || token2 <= token1 {
		t.Fatalf("newer claim: token=%d err=%v", token2, err)
	}

	// Scheduler-health needs no stats cache — generation succeeds, and the
	// fenced persist is what must reject.
	_, schedErr := eng.run(ctx, KindSchedulerHealth, token1)

	Convey("Given a hygiene scheduler holding a superseded fencing token", t, func() {
		Convey("Then the scheduled report persist is rejected", func() {
			So(errors.Is(schedErr, ErrSuperseded), ShouldBeTrue)
			So(rc.Exists(ctx, reportKey(KindSchedulerHealth)).Val(), ShouldEqual, 0)
		})
		Convey("And the operator-triggered Run (token 0) still writes — any replica may generate on demand", func() {
			rep, err := eng.Run(ctx, KindSchedulerHealth)
			So(err, ShouldBeNil)
			So(rep, ShouldNotBeNil)
			So(rc.Exists(ctx, reportKey(KindSchedulerHealth)).Val(), ShouldEqual, 1)
		})
	})
}
