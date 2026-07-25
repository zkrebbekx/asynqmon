package stats

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Attention-engine integration tests against a real Redis on DB 14 (this
// package's reserved DB — see engine_test.go). Fixtures are seeded through
// the real asynq client/server wherever the state is producible that way;
// the documented exceptions are:
//   - the expired lease entry (ORPHANS): authentic creation needs a worker
//     killed mid-task plus a 30s lease + grace wait
//   - the 10,000-entry archived set (ARCHIVE_TRIM): authentic creation needs
//     10k Archive calls; the layout tripwire (TestMirroredKeyFormats) covers
//     key placement, so a raw ZADD of the right shape suffices here
// Skipped when Redis does not answer PING.
// ****************************************************************************

// findingFor returns the finding for (queue, detector), or nil.
func findingFor(rep *AttentionReport, queue, detector string) *AttentionFinding {
	for i := range rep.Findings {
		if rep.Findings[i].Queue == queue && rep.Findings[i].Detector == detector {
			return &rep.Findings[i]
		}
	}
	return nil
}

// TestAttentionDetectors exercises all eight instantaneous detectors, the
// 2-sweep debounce, since persistence, auto-clear, and standby serving.
func TestAttentionDetectors(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	// q_nocons: pending work, nobody listening → NO_CONSUMERS (debounced).
	for i := 0; i < 2; i++ {
		if _, err := client.Enqueue(asynq.NewTask("at:wait", nil), asynq.Queue("q_nocons")); err != nil {
			t.Fatalf("enqueue q_nocons: %v", err)
		}
	}

	// q_orphan: one pending task (puts the queue in asynq:queues through the
	// real client) plus a hand-built expired lease entry (documented
	// exception, see file header).
	if _, err := client.Enqueue(asynq.NewTask("at:orphanhost", nil), asynq.Queue("q_orphan")); err != nil {
		t.Fatalf("enqueue q_orphan: %v", err)
	}
	if err := rc.ZAdd(ctx, leaseKey("q_orphan"),
		redis.Z{Score: float64(time.Now().Add(-2 * time.Minute).Unix()), Member: "dead-worker-task"},
	).Err(); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}

	// q_pastdue: a scheduled task whose process-at time passes before the
	// sweep; no asynq server consumes this queue, so no forwarder moves it —
	// exactly the stall PAST_DUE exposes.
	if _, err := client.Enqueue(asynq.NewTask("at:late", nil), asynq.Queue("q_pastdue"), asynq.ProcessIn(200*time.Millisecond)); err != nil {
		t.Fatalf("enqueue q_pastdue: %v", err)
	}

	// q_paused: paused through the real inspector (which stores the pause
	// time); the test threshold is tiny so "paused long" trips immediately.
	if _, err := client.Enqueue(asynq.NewTask("at:idle", nil), asynq.Queue("q_paused")); err != nil {
		t.Fatalf("enqueue q_paused: %v", err)
	}
	if err := insp.PauseQueue("q_paused"); err != nil {
		t.Fatalf("pause q_paused: %v", err)
	}

	// q_trim: archived set at exactly asynq's 10k cap (documented raw-write
	// exception, see file header).
	if _, err := client.Enqueue(asynq.NewTask("at:trimhost", nil), asynq.Queue("q_trim")); err != nil {
		t.Fatalf("enqueue q_trim: %v", err)
	}
	trimEntries := make([]redis.Z, maxArchiveSize)
	diedAt := time.Now().Add(-24 * time.Hour).Unix()
	for i := range trimEntries {
		trimEntries[i] = redis.Z{Score: float64(diedAt + int64(i)), Member: fmt.Sprintf("dead-%d", i)}
	}
	if err := rc.ZAdd(ctx, archivedKey("q_trim"), trimEntries...).Err(); err != nil {
		t.Fatalf("seed archived cap: %v", err)
	}

	// q_group: one aggregating task via the real client; the stall threshold
	// is tiny so its group trips after a short sleep.
	if _, err := client.Enqueue(asynq.NewTask("at:grouped", nil), asynq.Queue("q_group"), asynq.Group("g_slow")); err != nil {
		t.Fatalf("enqueue q_group: %v", err)
	}

	// q_storm: a real asynq server fails a task; RetryDelayFunc schedules the
	// retry 2 minutes out — inside the 5m storm window — and the threshold of
	// 1 makes a single retry a storm. The live server also gives this queue a
	// consumer, so q_storm never trips NO_CONSUMERS.
	srv := asynq.NewServer(testAsynqOpt(), asynq.Config{
		Concurrency: 1,
		Queues:      map[string]int{"q_storm": 1},
		LogLevel:    asynq.FatalLevel,
		RetryDelayFunc: func(n int, e error, task *asynq.Task) time.Duration {
			return 2 * time.Minute
		},
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc("at:fail", func(context.Context, *asynq.Task) error { return errors.New("boom") })
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting asynq server: %v", err)
	}
	defer srv.Shutdown()
	if _, err := client.Enqueue(asynq.NewTask("at:fail", nil), asynq.Queue("q_storm"), asynq.MaxRetry(3)); err != nil {
		t.Fatalf("enqueue failing task: %v", err)
	}
	waitFor(t, 15*time.Second, "task to reach retry state", func() bool {
		qi, err := insp.GetQueueInfo("q_storm")
		return err == nil && qi.Retry == 1
	})

	// Let the group entry age past the stall threshold, the scheduled task
	// fall past due, and the pending tasks exceed the tiny SLO.
	time.Sleep(600 * time.Millisecond)

	engine := NewEngine(Config{
		RedisClient:         rc,
		Inspector:           insp,
		Logf:                silentLogf,
		PendingAgeSLO:       time.Millisecond, // any pending task trips PENDING_AGE
		RetryStormThreshold: 1,
		PausedLongAfter:     time.Millisecond,
		GroupStallAfter:     time.Millisecond,
	})

	sweep1Err := engine.SweepNow(ctx)
	rep1, read1Err := engine.ReadAttention(ctx)
	sweep2Err := engine.SweepNow(ctx)
	rep2, read2Err := engine.ReadAttention(ctx)

	Convey("Given a fleet seeded to trip every instantaneous detector", t, func() {
		So(sweep1Err, ShouldBeNil)
		So(read1Err, ShouldBeNil)
		So(sweep2Err, ShouldBeNil)
		So(read2Err, ShouldBeNil)

		Convey("Then the report footer states the phase-4 detector census", func() {
			So(rep2.DetectorsLive, ShouldEqual, 9)
			So(rep2.DetectorsLearning, ShouldEqual, 0)
			So(rep2.UpdatedAt, ShouldNotBeEmpty)
		})

		Convey("When the first sweep completes (debounced detectors still counting)", func() {
			Convey("Then NO_CONSUMERS and ORPHANS are withheld — one sweep is not two", func() {
				So(findingFor(rep1, "q_nocons", DetectorNoConsumers), ShouldBeNil)
				So(findingFor(rep1, "q_orphan", DetectorOrphans), ShouldBeNil)
			})
			Convey("Then the instantaneous detectors already report", func() {
				So(findingFor(rep1, "q_nocons", DetectorPendingAge), ShouldNotBeNil)
				So(findingFor(rep1, "q_pastdue", DetectorPastDue), ShouldNotBeNil)
				So(findingFor(rep1, "q_paused", DetectorPausedLong), ShouldNotBeNil)
				So(findingFor(rep1, "q_trim", DetectorArchiveTrim), ShouldNotBeNil)
				So(findingFor(rep1, "q_group", DetectorGroupStall), ShouldNotBeNil)
				So(findingFor(rep1, "q_storm", DetectorRetryStorm), ShouldNotBeNil)
			})
		})

		Convey("When the second sweep completes", func() {
			Convey("Then NO_CONSUMERS reports with the contract's field shape", func() {
				f := findingFor(rep2, "q_nocons", DetectorNoConsumers)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 5)
				So(f.Value, ShouldEqual, 2) // the pending count is the key number
				So(f.Sentence, ShouldContainSubstring, "2 pending, zero live consumers")
				So(f.Sentence, ShouldContainSubstring, "work will not run")
				So(f.Chips, ShouldResemble, []string{"NO CONSUMERS"})
				So(f.Since, ShouldNotBeEmpty)
				So(f.SuggestedQuery, ShouldEqual, "state=pending queue=q_nocons")
			})

			Convey("Then ORPHANS reports after persisting across two sweeps", func() {
				f := findingFor(rep2, "q_orphan", DetectorOrphans)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 4)
				So(f.Value, ShouldEqual, 1)
				So(f.SuggestedQuery, ShouldEqual, "state=active queue=q_orphan orphaned")
			})

			Convey("Then PENDING_AGE carries the age as its value and escalates past 6x SLO", func() {
				f := findingFor(rep2, "q_nocons", DetectorPendingAge)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 4) // age >> 6x the 1ms test SLO
				So(f.Value, ShouldBeGreaterThanOrEqualTo, 0)
				So(f.SuggestedQuery, ShouldEqual, "state=pending queue=q_nocons pending_age>1ms")
			})

			Convey("Then RETRY_STORM counts the 2m-out retry inside the 5m window", func() {
				f := findingFor(rep2, "q_storm", DetectorRetryStorm)
				So(f, ShouldNotBeNil)
				So(f.Value, ShouldEqual, 1)
				So(f.Severity, ShouldEqual, 3) // 1 is 1x the threshold, not 10x
				So(f.SuggestedQuery, ShouldEqual, "state=retry queue=q_storm next_run<5m")
				// And the live server means the storm queue never trips
				// NO_CONSUMERS.
				So(findingFor(rep2, "q_storm", DetectorNoConsumers), ShouldBeNil)
			})

			Convey("Then PAST_DUE reports the stalled forwarder", func() {
				f := findingFor(rep2, "q_pastdue", DetectorPastDue)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 4)
				So(f.Value, ShouldEqual, 1)
				So(f.SuggestedQuery, ShouldEqual, "state=scheduled queue=q_pastdue past_due")
			})

			Convey("Then PAUSED_LONG uses asynq's stored pause time, not an invention", func() {
				f := findingFor(rep2, "q_paused", DetectorPausedLong)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 1)
				So(f.SuggestedQuery, ShouldEqual, "name=q_paused paused=true")
			})

			Convey("Then ARCHIVE_TRIM fires exactly at asynq's 10k cap", func() {
				f := findingFor(rep2, "q_trim", DetectorArchiveTrim)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 2)
				So(f.Value, ShouldEqual, 10000)
				So(f.Sentence, ShouldContainSubstring, "10,000")
			})

			Convey("Then GROUP_STALL names the stalled group", func() {
				f := findingFor(rep2, "q_group", DetectorGroupStall)
				So(f, ShouldNotBeNil)
				So(f.Severity, ShouldEqual, 3)
				So(f.Sentence, ShouldContainSubstring, `"g_slow"`)
				So(f.SuggestedQuery, ShouldEqual, "state=aggregating queue=q_group group=g_slow")
			})

			Convey("Then since persists across sweeps instead of resetting", func() {
				f1 := findingFor(rep1, "q_pastdue", DetectorPastDue)
				f2 := findingFor(rep2, "q_pastdue", DetectorPastDue)
				So(f1, ShouldNotBeNil)
				So(f2, ShouldNotBeNil)
				So(f2.Since, ShouldEqual, f1.Since)
			})

			Convey("Then findings rank by severity first", func() {
				So(rep2.Findings, ShouldNotBeEmpty)
				for i := 1; i < len(rep2.Findings); i++ {
					So(rep2.Findings[i-1].Severity, ShouldBeGreaterThanOrEqualTo, rep2.Findings[i].Severity)
				}
				So(rep2.Findings[0].Detector, ShouldEqual, DetectorNoConsumers)
			})

			Convey("Then a standby replica serves the identical report from the cache", func() {
				standby := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
				got, err := standby.ReadAttention(ctx)
				So(err, ShouldBeNil)
				So(got.UpdatedAt, ShouldEqual, rep2.UpdatedAt)
				So(got.Findings, ShouldResemble, rep2.Findings)
			})
		})

		// Keep this leaf last: it mutates the fleet.
		Convey("When conditions end and the next sweep runs", func() {
			So(insp.UnpauseQueue("q_paused"), ShouldBeNil)
			So(rc.ZRem(ctx, leaseKey("q_orphan"), "dead-worker-task").Err(), ShouldBeNil)
			So(engine.SweepNow(ctx), ShouldBeNil)
			rep3, err := engine.ReadAttention(ctx)
			So(err, ShouldBeNil)
			Convey("Then their findings auto-clear while unrelated ones remain", func() {
				So(findingFor(rep3, "q_paused", DetectorPausedLong), ShouldBeNil)
				So(findingFor(rep3, "q_orphan", DetectorOrphans), ShouldBeNil)
				So(findingFor(rep3, "q_trim", DetectorArchiveTrim), ShouldNotBeNil)
				So(findingFor(rep3, "q_nocons", DetectorNoConsumers), ShouldNotBeNil)
			})
		})
	})
}

// TestAttentionReportCap verifies the rail cap: state is evaluated for every
// queue but the published report carries at most 20 ranked findings.
func TestAttentionReportCap(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		q := fmt.Sprintf("capq-%02d", i)
		if _, err := client.Enqueue(asynq.NewTask("cap:task", nil), asynq.Queue(q)); err != nil {
			t.Fatalf("enqueue %s: %v", q, err)
		}
	}
	time.Sleep(50 * time.Millisecond) // age every pending task past the tiny SLO

	engine := NewEngine(Config{
		RedisClient:   rc,
		Inspector:     insp,
		Logf:          silentLogf,
		PendingAgeSLO: time.Millisecond,
	})
	sweepErr := engine.SweepNow(ctx)
	rep, readErr := engine.ReadAttention(ctx)

	Convey("Given 25 queues that all trip PENDING_AGE", t, func() {
		So(sweepErr, ShouldBeNil)
		So(readErr, ShouldBeNil)
		Convey("When the report is read", func() {
			Convey("Then it is capped at 20 ranked findings", func() {
				So(len(rep.Findings), ShouldEqual, 20)
			})
		})
	})
}

// TestAttentionEvaluatorDebounce is a PURE unit test (no Redis): the
// evaluator is a pure function over snapshots plus its own cross-sweep
// state, so debounce, since persistence across wall-clock seconds, and
// debounce restart after a clear are all provable deterministically —
// something the integration test cannot pin down when its sweeps land
// within one RFC3339 second.
func TestAttentionEvaluatorDebounce(t *testing.T) {
	Convey("Given an evaluator and a queue with pending work and no consumers", t, func() {
		ev := newAttentionEvaluator(attentionConfig{
			pendingAgeSLO:       time.Hour, // out of the way
			retryStormThreshold: 1000,
			pausedLongAfter:     time.Hour,
			groupStallAfter:     time.Hour,
			orphanGrace:         30 * time.Second,
		})
		t0 := time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)
		snaps := map[string]*QueueSnapshot{"q": {Queue: "q", Pending: 5}}

		Convey("When three sweeps observe the condition seconds apart", func() {
			rep1 := ev.evaluate(snaps, nil, nil, t0)
			rep2 := ev.evaluate(snaps, nil, nil, t0.Add(5*time.Second))
			rep3 := ev.evaluate(snaps, nil, nil, t0.Add(10*time.Second))

			Convey("Then the first sweep withholds (debounce) and later sweeps report", func() {
				So(findingFor(rep1, "q", DetectorNoConsumers), ShouldBeNil)
				So(findingFor(rep2, "q", DetectorNoConsumers), ShouldNotBeNil)
				So(findingFor(rep3, "q", DetectorNoConsumers), ShouldNotBeNil)
			})
			Convey("Then since stays the FIRST observation time across sweeps", func() {
				f2 := findingFor(rep2, "q", DetectorNoConsumers)
				f3 := findingFor(rep3, "q", DetectorNoConsumers)
				So(f2.Since, ShouldEqual, t0.Format(time.RFC3339))
				So(f3.Since, ShouldEqual, t0.Format(time.RFC3339))
				So(f3.Sentence, ShouldContainSubstring, "for 10s")
			})

			Convey("And when the condition clears and later returns", func() {
				healthy := map[string]*QueueSnapshot{"q": {Queue: "q", Pending: 5, Consumers: 1}}
				rep4 := ev.evaluate(healthy, nil, nil, t0.Add(15*time.Second))
				t5 := t0.Add(20 * time.Second)
				rep5 := ev.evaluate(snaps, nil, nil, t5)
				rep6 := ev.evaluate(snaps, nil, nil, t5.Add(5*time.Second))

				Convey("Then it auto-clears and the debounce (and since) restart from scratch", func() {
					So(findingFor(rep4, "q", DetectorNoConsumers), ShouldBeNil)
					So(findingFor(rep5, "q", DetectorNoConsumers), ShouldBeNil) // one sweep again
					f6 := findingFor(rep6, "q", DetectorNoConsumers)
					So(f6, ShouldNotBeNil)
					So(f6.Since, ShouldEqual, t5.Format(time.RFC3339))
				})
			})
		})
	})
}

// TestAttentionEvaluatorGroupCarry is a PURE unit test of the GROUP_STALL
// sticky rule: a queue whose groups were not examined this sweep (bounded
// rotation looked elsewhere) replays its previous finding instead of
// auto-clearing on ignorance; a fresh healthy observation clears it.
func TestAttentionEvaluatorGroupCarry(t *testing.T) {
	Convey("Given a grouped queue whose oldest member is past the stall threshold", t, func() {
		ev := newAttentionEvaluator(attentionConfig{
			pendingAgeSLO:       time.Hour,
			retryStormThreshold: 1000,
			pausedLongAfter:     time.Hour,
			groupStallAfter:     time.Minute,
			orphanGrace:         30 * time.Second,
		})
		t0 := time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)
		snaps := map[string]*QueueSnapshot{"gq": {Queue: "gq", Groups: 1}}
		stalled := map[string]groupStallObs{"gq": {group: "g1", oldestSince: t0.Add(-10 * time.Minute)}}

		Convey("When an examined sweep finds the stall", func() {
			rep1 := ev.evaluate(snaps, stalled, nil, t0)
			f1 := findingFor(rep1, "gq", DetectorGroupStall)
			So(f1, ShouldNotBeNil)

			Convey("And the next sweep's bounded reader does not revisit the queue", func() {
				rep2 := ev.evaluate(snaps, nil, nil, t0.Add(5*time.Second))
				Convey("Then the previous finding is replayed verbatim, not cleared", func() {
					f2 := findingFor(rep2, "gq", DetectorGroupStall)
					So(f2, ShouldNotBeNil)
					So(*f2, ShouldResemble, *f1)
				})

				Convey("And a later examined-healthy sweep clears it", func() {
					healthy := map[string]groupStallObs{"gq": {}} // examined, no members
					rep3 := ev.evaluate(snaps, healthy, nil, t0.Add(10*time.Second))
					So(findingFor(rep3, "gq", DetectorGroupStall), ShouldBeNil)
				})
			})
		})
	})
}

// TestAttentionNotReady covers the standby-before-first-sweep path.
func TestAttentionNotReady(t *testing.T) {
	rc := testRedis(t)
	_, insp := testClientInspector(t)
	ctx := context.Background()

	Convey("Given an engine that has never swept and an empty cache", t, func() {
		engine := NewEngine(Config{RedisClient: rc, Inspector: insp, Logf: silentLogf})
		Convey("When the attention report is read", func() {
			_, err := engine.ReadAttention(ctx)
			Convey("Then it reports not-ready instead of fabricating an empty healthy report", func() {
				So(errors.Is(err, ErrNotReady), ShouldBeTrue)
			})
		})
	})
}
