package stats

import (
	"context"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Redis-backed tests for the ViewTracker publish path (rotation.go). They use
// the shared DB-14 test client (stats/engine_test.go testRedis), which the
// suite already flushes. Skipped when Redis does not answer PING.
// ****************************************************************************

// ----------------------------------------------------------------------------
// #33: both the shared viewed zset and the in-process throttle map are
// bounded, so a flood of distinct queue names cannot grow Redis memory or
// make each request O(n). Uses the shared DB-14 test client (flushed).
// ----------------------------------------------------------------------------

func TestViewTrackerCaps(t *testing.T) {
	rc := testRedis(t)
	ctx := context.Background()

	// 10,000 distinct names, each one view: the throttle never fires, so
	// every call publishes.
	v := NewViewTracker(rc, nil)
	for i := 0; i < 10000; i++ {
		v.MarkViewed(ctx, fmt.Sprintf("flood-%05d", i))
	}
	card, cardErr := rc.ZCard(ctx, viewedKey).Result()

	v.mu.Lock()
	tracked := len(v.last)
	v.mu.Unlock()

	// The newest names survive the cap; the oldest are trimmed.
	newest, newestErr := rc.ZScore(ctx, viewedKey, "flood-09999").Result()
	oldest := rc.ZScore(ctx, viewedKey, "flood-00000").Err()

	Convey("Given a ViewTracker marking 10,000 distinct queue names", t, func() {
		Convey("Then the shared zset stays at the cap", func() {
			So(cardErr, ShouldBeNil)
			So(card, ShouldBeLessThanOrEqualTo, int64(viewedSetCap))
			So(card, ShouldEqual, int64(viewedSetCap))
		})
		Convey("Then the in-process throttle map stays at the cap", func() {
			So(tracked, ShouldBeLessThanOrEqualTo, viewedTrackerCap)
		})
		Convey("Then the newest views survive and the oldest are trimmed", func() {
			So(newestErr, ShouldBeNil)
			So(newest, ShouldBeGreaterThan, 0)
			So(oldest, ShouldEqual, redis.Nil)
		})
	})
}
