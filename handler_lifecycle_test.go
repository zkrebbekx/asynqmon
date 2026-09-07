package asynqmon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Constructor lifecycle tests (#41) on DB 14: New must return without waiting
// for Redis, and the system views must still be seeded once Redis answers.
// ****************************************************************************

const handlerLifecycleTestRedisDB = 14

func handlerLifecycleRedisAddr() string {
	if v := os.Getenv("ASYNQMON_TEST_REDIS_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:6379"
}

// TestNewDoesNotBlockOnRedis is the #41 regression test. New used to call
// seedSystemViews synchronously, so with Redis unreachable the process could
// not bind its port until go-redis exhausted DialTimeout x (1 + MaxRetries):
// about 10s blackholed. New must now return at once.
func TestNewDoesNotBlockOnRedis(t *testing.T) {
	// A port with no listener: dialing it fails fast, but go-redis still
	// retries, which is what used to accumulate inside New.
	start := time.Now()
	h := New(Options{
		RedisConnOpt: asynq.RedisClientOpt{
			Addr:        "127.0.0.1:1",
			DialTimeout: 2 * time.Second,
		},
		// The background engines take leases; leaving them on proves none
		// of them blocks the constructor either.
	})
	elapsed := time.Since(start)

	// The SPA needs no Redis and must serve while Redis is down.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	closeStart := time.Now()
	closeErr := h.Close()
	closeElapsed := time.Since(closeStart)

	Convey("Given an unreachable redis address (#41)", t, func() {
		Convey("When New builds the handler", func() {
			Convey("Then it returns in under 100ms instead of waiting for redis", func() {
				So(h, ShouldNotBeNil)
				So(elapsed, ShouldBeLessThan, 100*time.Millisecond)
			})
			Convey("Then the SPA still serves, because it needs no redis", func() {
				So(rec.Code, ShouldEqual, http.StatusOK)
			})
		})
		Convey("When the handler closes", func() {
			Convey("Then the seeding goroutine stops with it", func() {
				So(closeErr, ShouldBeNil)
				So(closeElapsed, ShouldBeLessThan, 5*time.Second)
			})
		})
	})
}

// TestSystemViewsSeededInBackground proves the seed still happens: New
// returns immediately and the shipped views appear in Redis shortly after.
func TestSystemViewsSeededInBackground(t *testing.T) {
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: handlerLifecycleRedisAddr(), DB: handlerLifecycleTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", handlerLifecycleRedisAddr(), err)
	}
	defer rc.Close()
	if err := rc.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing db %d: %v", handlerLifecycleTestRedisDB, err)
	}

	start := time.Now()
	h := New(Options{
		RedisConnOpt: asynq.RedisClientOpt{
			Addr: handlerLifecycleRedisAddr(),
			DB:   handlerLifecycleTestRedisDB,
		},
		StatsDisabled:      true,
		HygieneDisabled:    true,
		ErrorIndexDisabled: true,
		JobsDisabled:       true,
	})
	elapsed := time.Since(start)
	defer h.Close()

	seeded := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rc.ZCard(ctx, "asynqmon:views:index").Val() == 5 {
			seeded = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	Convey("Given a reachable redis (#41)", t, func() {
		Convey("When New builds the handler", func() {
			Convey("Then it returns without waiting for the seed", func() {
				So(elapsed, ShouldBeLessThan, 100*time.Millisecond)
			})
			Convey("Then the five system views are seeded shortly after", func() {
				So(seeded, ShouldBeTrue)
			})
		})
	})
}

// TestSeedSystemViewsInBackgroundRetries covers the backoff path: a store
// that fails once and then succeeds must still end up seeded, and a canceled
// context must stop the goroutine.
func TestSeedSystemViewsInBackgroundRetries(t *testing.T) {
	Convey("Given a view store that fails its first seed (#41)", t, func() {
		store := &flakyViewStore{failures: 1, views: map[string]View{}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := seedSystemViewsInBackground(ctx, store)

		Convey("When the seeder retries", func() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the seeding goroutine never finished")
			}
			Convey("Then the views are seeded on the second attempt", func() {
				So(store.attempts(), ShouldBeGreaterThan, 1)
				So(len(store.all()), ShouldEqual, len(systemViews))
			})
		})
	})

	Convey("Given a view store that never succeeds (#41)", t, func() {
		store := &flakyViewStore{failures: 1 << 30, views: map[string]View{}}
		ctx, cancel := context.WithCancel(context.Background())
		done := seedSystemViewsInBackground(ctx, store)

		Convey("When the handler closes", func() {
			cancel()
			var stopped bool
			select {
			case <-done:
				stopped = true
			case <-time.After(5 * time.Second):
			}
			Convey("Then the seeding goroutine stops instead of retrying forever", func() {
				So(stopped, ShouldBeTrue)
			})
		})
	})
}
