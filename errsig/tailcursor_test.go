package errsig

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
// Same-second cursor test for the archived tail. asynq scores the archived
// zset in whole seconds, so two tasks can share a score. The (score, id)
// cursor skips a tie it already served; a task archived later in the SAME
// second whose id sorts below the cursor id would be lost forever. The sweep
// therefore stops one TailSafetyLag short of the current second. Real Redis,
// DB 6 (shared with fence_test.go and budget_test.go).
// ****************************************************************************

// archiveTwoTasks enqueues two tasks with the given ids into qname and runs a
// handler that fails both, so asynq archives them with a real LastErr.
func archiveTwoTasks(t *testing.T, insp *asynq.Inspector, client *asynq.Client, qname, msg, idLow, idHigh string) {
	t.Helper()
	for _, id := range []string{idLow, idHigh} {
		if _, err := client.Enqueue(asynq.NewTask("errsig:tie", []byte(msg)),
			asynq.Queue(qname), asynq.MaxRetry(0), asynq.TaskID(id)); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB},
		asynq.Config{Concurrency: 2, Queues: map[string]int{qname: 1}, LogLevel: asynq.FatalLevel},
	)
	mux := asynq.NewServeMux()
	mux.HandleFunc("errsig:tie", func(ctx context.Context, task *asynq.Task) error {
		return fmt.Errorf("%s", string(task.Payload()))
	})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting failing server: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		qi, err := insp.GetQueueInfo(qname)
		if err == nil && qi.Archived == 2 {
			break
		}
		if time.Now().After(deadline) {
			srv.Shutdown()
			t.Fatalf("timed out waiting for two archived tasks")
		}
		time.Sleep(50 * time.Millisecond)
	}
	srv.Shutdown()
}

func TestTailCursorKeepsSameSecondEntries(t *testing.T) {
	rc := fenceTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	const (
		qname  = "tieq"
		msg    = "same second failure"
		idLow  = "aaa-task" // sorts BELOW the cursor id
		idHigh = "zzz-task"
	)
	archiveTwoTasks(t, insp, client, qname, msg, idLow, idHigh)

	// Put both tasks in the same died-at second and hide the low id, so the
	// sweep below sees only the high id first — the exact shape that lost
	// the second task before the safety lag existed.
	second := time.Now().Unix()
	if err := rc.ZAdd(ctx, archivedKey(qname), redis.Z{Score: float64(second), Member: idHigh}).Err(); err != nil {
		t.Fatalf("rescoring %s: %v", idHigh, err)
	}
	if err := rc.ZRem(ctx, archivedKey(qname), idLow).Err(); err != nil {
		t.Fatalf("removing %s: %v", idLow, err)
	}

	fake := time.Unix(second, 0)
	ix := NewIndexer(Config{
		RedisClient: rc,
		Inspector:   insp,
		Now:         func() time.Time { return fake },
		Logf:        func(string, ...interface{}) {},
	})

	// Sweep 1 runs inside the same second the task was archived in.
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	indexedAfterFirst := rc.ZCard(ctx, indexKey).Val()

	// The second task lands in that same second, after sweep 1.
	if err := rc.ZAdd(ctx, archivedKey(qname), redis.Z{Score: float64(second), Member: idLow}).Err(); err != nil {
		t.Fatalf("re-adding %s: %v", idLow, err)
	}

	// Sweep 2 runs after the safety lag has passed.
	fake = time.Unix(second+3, 0)
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	sigs, _, err := ReadIndex(ctx, rc)
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}

	Convey("Given two tasks archived in the same died-at second", t, func() {
		Convey("When a sweep runs inside that second", func() {
			Convey("Then it indexes nothing — the cursor must not enter a second that can still grow", func() {
				So(indexedAfterFirst, ShouldEqual, int64(0))
			})
		})
		Convey("When the next sweep runs after the safety lag", func() {
			Convey("Then both tasks are indexed, including the id that sorts below the other", func() {
				So(len(sigs), ShouldEqual, 1)
				So(sigs[0].ArchivedCount, ShouldEqual, int64(2))
			})
			Convey("Then the cursor parked on the highest id of that second", func() {
				cur := decodeCursor(rc.HGet(ctx, cursorsKey, qname).Val())
				So(cur.valid, ShouldBeTrue)
				So(cur.score, ShouldEqual, second)
				So(cur.id, ShouldEqual, idHigh)
			})
		})
	})
}
