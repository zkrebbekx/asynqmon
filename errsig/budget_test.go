package errsig

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Command-budget rotation test (§5.7 governor): a tail sweep whose fleet
// exceeds the per-tick budget covers a prefix, resumes from the rotation
// cursor next tick, and the meta hash's fleet-wide numbers accumulate across
// partial sweeps instead of shrinking to the swept subset. Same real-Redis
// DB 6 arrangement as fence_test.go.
// ****************************************************************************

func TestTailSweepBudgetRotation(t *testing.T) {
	rc := fenceTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB}
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { insp.Close() })

	// 300 queues with empty archives and a failed-today counter of 1 each:
	// per-queue sweep cost is 3 meta + 1 tail ZRANGE = 4 commands, so the
	// fleet costs ~1200 — beyond the 800-command floor one sweep gets.
	const numQueues = 300
	now := time.Now()
	for i := 0; i < numQueues; i++ {
		q := fmt.Sprintf("bq%03d", i)
		if err := rc.SAdd(ctx, allQueuesKey, q).Err(); err != nil {
			t.Fatalf("seeding queue set: %v", err)
		}
		if err := rc.Set(ctx, FailedTodayKey(q, now), "1", time.Hour).Err(); err != nil {
			t.Fatalf("seeding failed counter: %v", err)
		}
	}

	ix := NewIndexer(Config{
		RedisClient: rc,
		Inspector:   insp,
		// Budget floor (4×metaChunkSize = 800 commands) applies: CommandBudget
		// and TailInterval land below it on purpose.
		CommandBudget: 1,
		TailInterval:  time.Second,
		Logf:          func(string, ...interface{}) {},
	})

	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	meta1, err := readMeta(ctx, rc)
	if err != nil {
		t.Fatalf("reading meta after sweep 1: %v", err)
	}
	rot1 := ix.tailRot

	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	meta2, err := readMeta(ctx, rc)
	if err != nil {
		t.Fatalf("reading meta after sweep 2: %v", err)
	}

	Convey("Given a fleet larger than one sweep's command budget", t, func() {
		Convey("The first sweep covers a strict prefix and parks a rotation cursor", func() {
			So(meta1.FailedTodayTotal, ShouldBeGreaterThan, 0)
			So(meta1.FailedTodayTotal, ShouldBeLessThan, int64(numQueues))
			So(rot1, ShouldNotBeEmpty)
		})
		Convey("The next sweep resumes from the cursor and the fleet-wide total accumulates to completion", func() {
			So(meta2.FailedTodayTotal, ShouldEqual, int64(numQueues))
		})
	})
}

// TestSpendCountsMergePhase: the merge phase and the retry sample are part
// of the reported command spend (#52.5). Same real-Redis DB 6 arrangement.
func TestSpendCountsMergePhase(t *testing.T) {
	rc := fenceTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: fenceTestRedisAddr, DB: fenceTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	if _, err := client.Enqueue(asynq.NewTask("t:x", nil), asynq.Queue("spq")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ix := NewIndexer(Config{RedisClient: rc, Inspector: insp, Logf: func(string, ...interface{}) {}})
	before := ix.Spend()
	if err := ix.SweepTailNow(ctx); err != nil {
		t.Fatalf("tail sweep: %v", err)
	}
	if err := ix.SampleRetryNow(ctx); err != nil {
		t.Fatalf("retry sample: %v", err)
	}
	after := ix.Spend()

	Convey("Given an indexer that never ran", t, func() {
		Convey("Then its reported spend is zero", func() {
			So(before.TailCommands, ShouldEqual, 0)
			So(before.SampleCommands, ShouldEqual, 0)
			So(before.TailAt.IsZero(), ShouldBeTrue)
		})
	})
	Convey("Given one tail sweep and one retry sample", t, func() {
		Convey("Then the tail spend includes the merge phase", func() {
			So(after.TailMergeCommands, ShouldBeGreaterThan, 0)
			So(after.TailCommands, ShouldBeGreaterThan, after.TailMergeCommands)
			So(after.TailBudget, ShouldBeGreaterThan, 0)
			So(after.TailAt.IsZero(), ShouldBeFalse)
		})
		Convey("Then the sample spend includes its union reads and writes", func() {
			So(after.SampleMergeCommands, ShouldBeGreaterThan, 0)
			So(after.SampleCommands, ShouldBeGreaterThanOrEqualTo, after.SampleMergeCommands)
			So(after.SampleAt.IsZero(), ShouldBeFalse)
		})
		Convey("Then the budget is reported with the spend", func() {
			So(after.Budget, ShouldEqual, defaultCommandBudget)
		})
	})
}
