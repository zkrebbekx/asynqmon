package errsig

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
// Fencing test for the errsig indexer role (§5.13, phase 12) against a real
// Redis on 127.0.0.1:6379, DB 6 (unused elsewhere: root suites use 7-13,
// stats 14, leasefence 15). Skipped when Redis does not answer PING.
// ****************************************************************************

const (
	fenceTestRedisAddr = "127.0.0.1:6379"
	fenceTestRedisDB   = 6
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

// TestIndexerFencing: a stale-token indexer's write is rejected and it stands
// down; explicit off-lease runs (token 0) still write.
func TestIndexerFencing(t *testing.T) {
	rc := fenceTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	// Seed one queue so the tail sweep has something to walk.
	if _, err := client.Enqueue(asynq.NewTask("t:x", nil), asynq.Queue("eq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ix := NewIndexer(Config{RedisClient: rc, Inspector: insp, Logf: func(string, ...interface{}) {}})

	// Claim as this indexer (token1), then a newer replica supersedes it.
	token1, err := ix.fence.Acquire(ctx, ix.cfg.InstanceID, time.Minute)
	if err != nil || token1 == 0 {
		t.Fatalf("acquire: token=%d err=%v", token1, err)
	}
	atomic.StoreInt64(&ix.token, token1)
	atomic.StoreInt32(&ix.holding, 1)
	if err := ix.fence.Release(ctx, ix.cfg.InstanceID); err != nil {
		t.Fatalf("release: %v", err)
	}
	token2, err := ix.fence.Acquire(ctx, "newer-replica", time.Minute)
	if err != nil || token2 <= token1 {
		t.Fatalf("newer claim: token=%d err=%v", token2, err)
	}

	sweepErr := ix.SweepTailNow(ctx)

	Convey("Given an indexer holding a superseded fencing token", t, func() {
		Convey("Then the tail sweep's index write is rejected", func() {
			So(errors.Is(sweepErr, ErrSuperseded), ShouldBeTrue)
			So(rc.Exists(ctx, metaKey).Val(), ShouldEqual, 0)
		})
		Convey("Then the ex-holder stood down", func() {
			So(ix.LeaseHeld(), ShouldBeFalse)
			So(atomic.LoadInt64(&ix.token), ShouldEqual, 0)
		})
		Convey("And an explicitly off-lease sweep (token 0) still writes — the documented *Now semantics", func() {
			So(ix.SweepTailNow(ctx), ShouldBeNil)
			So(rc.Exists(ctx, metaKey).Val(), ShouldEqual, 1)
		})
	})
}
