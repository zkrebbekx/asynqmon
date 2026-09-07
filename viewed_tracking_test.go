package asynqmon

import (
	"context"
	"net/http"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// #33: an unauthenticated GET must not write an arbitrary queue name into the
// shared asynqmon:viewed zset. The router marks a queue viewed only after the
// wrapped handler answered 2xx.
//
// This suite owns Redis DB 3 and is the only suite that flushes it. Skipped
// when Redis does not answer PING.
// ****************************************************************************

const viewedTestRedisDB = 3

// viewedZSetKey mirrors stats.viewedKey (unexported there).
const viewedZSetKey = "asynqmon:viewed"

func newViewedTestRouter(t *testing.T) (http.Handler, *redis.Client) {
	t.Helper()
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: viewedTestRedisAddr, DB: viewedTestRedisDB})
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", viewedTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db %d: %v", viewedTestRedisDB, err)
	}
	opt := asynq.RedisClientOpt{Addr: viewedTestRedisAddr, DB: viewedTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	if _, err := client.Enqueue(asynq.NewTask("viewed:task", nil), asynq.Queue("real")); err != nil {
		t.Fatalf("seeding queue real: %v", err)
	}
	t.Cleanup(func() {
		client.Close()
		insp.Close()
		rc.Close()
	})
	return muxRouter(Options{}, rc, insp, nil, nil), rc
}

func TestMarkViewedOnlyAfter2xx(t *testing.T) {
	router, rc := newViewedTestRouter(t)
	ctx := context.Background()

	// 50 GETs of names that do not exist: none answers 2xx.
	unknown2xx := 0
	for i := 0; i < 50; i++ {
		w := doJSON(t, router, "GET", "/api/queues/ghost-"+string(rune('a'+i%26))+string(rune('a'+i/26)), nil, nil)
		if w.Code >= 200 && w.Code < 300 {
			unknown2xx++
		}
	}
	afterUnknown, unknownErr := rc.ZCard(ctx, viewedZSetKey).Result()

	// One GET of a real queue: it answers 200 and marks the queue.
	okRec := doJSON(t, router, "GET", "/api/queues/real", nil, nil)
	members, membersErr := rc.ZRange(ctx, viewedZSetKey, 0, -1).Result()

	Convey("Given the queue workspace routes on a fleet with one real queue", t, func() {
		Convey("When 50 GETs name queues that do not exist", func() {
			Convey("Then no request answers 2xx", func() {
				So(unknown2xx, ShouldEqual, 0)
			})
			Convey("Then no name reaches the shared viewed zset", func() {
				So(unknownErr, ShouldBeNil)
				So(afterUnknown, ShouldEqual, 0)
			})
		})
		Convey("When the GET names a real queue and answers 200", func() {
			Convey("Then the queue is marked viewed", func() {
				So(okRec.Code, ShouldEqual, http.StatusOK)
				So(membersErr, ShouldBeNil)
				So(members, ShouldResemble, []string{"real"})
			})
		})
	})
}

var viewedTestRedisAddr = testRedisAddrFromEnv()
