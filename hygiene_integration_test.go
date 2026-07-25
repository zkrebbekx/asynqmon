package asynqmon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/hibiken/asynqmon/errsig"
	"github.com/hibiken/asynqmon/hygiene"
	"github.com/hibiken/asynqmon/stats"
)

// ****************************************************************************
// End-to-end integration for the §3.10 hygiene reports — against a real
// Redis on 127.0.0.1:6379 DB 7 (shared with hygiene_handlers_test.go; same
// package, so the suites run serially and only DB 7 is ever flushed).
//
// A real fleet is seeded through the asynq client, a real worker server and
// real asynq schedulers, then all four reports are generated and their
// content asserted — including dead-queue detection and the retention
// histogram. The documented layout exceptions (attention_test.go precedent):
// backdating archived died-at / completed expiry scores and the paused-since
// value, which authentic creation would need real weeks for. Key placement
// stays tripwired by seeding the members themselves through asynq.
//
// Skipped when Redis does not answer PING.
// ****************************************************************************

// waitForCond polls until ok or the timeout elapses.
func waitForCond(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHygieneReportsIntegration(t *testing.T) {
	env := newHygieneTestEnv(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: hygieneTestRedisAddr, DB: hygieneTestRedisDB}
	client := asynq.NewClient(opt)
	t.Cleanup(func() { client.Close() })

	// ---- Worker server: consumes orders (succeeds) + payments (fails). ----
	handler := asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
		if task.Type() == "pay:capture" {
			return errors.New("card declined 4021")
		}
		return nil
	})
	srv := asynq.NewServer(opt, asynq.Config{
		Concurrency: 4,
		Queues:      map[string]int{"orders": 1, "payments": 1},
		Logger:      discardLogger{},
	})
	if err := srv.Start(handler); err != nil {
		t.Fatalf("starting asynq server: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	// ---- Scheduler B: registers one entry, then dies (the GONE corpse). ----
	schedB := asynq.NewScheduler(opt, nil)
	if _, err := schedB.Register("@every 1h", asynq.NewTask("cron:dead", nil), asynq.Queue("orders")); err != nil {
		t.Fatalf("registering cron:dead: %v", err)
	}
	if err := schedB.Start(); err != nil {
		t.Fatalf("starting scheduler B: %v", err)
	}

	// ---- Scheduler A: stays alive — retention-less + zero-consumer entry,
	// and a fully-healthy traceable entry. ----
	schedA := asynq.NewScheduler(opt, nil)
	if _, err := schedA.Register("@every 1h", asynq.NewTask("cron:noretention", nil), asynq.Queue("maintenance")); err != nil {
		t.Fatalf("registering cron:noretention: %v", err)
	}
	if _, err := schedA.Register("@every 1h", asynq.NewTask("cron:traceable", nil), asynq.Queue("orders"), asynq.Retention(time.Hour)); err != nil {
		t.Fatalf("registering cron:traceable: %v", err)
	}
	if err := schedA.Start(); err != nil {
		t.Fatalf("starting scheduler A: %v", err)
	}
	t.Cleanup(func() { schedA.Shutdown() })

	waitForCond(t, "all 3 scheduler entries visible", 10*time.Second, func() bool {
		entries, err := env.insp.SchedulerEntries()
		return err == nil && len(entries) == 3
	})

	// ---- Stats engine (leased sweeps not needed — SweepNow only). ----
	statsEngine := stats.NewEngine(stats.Config{RedisClient: env.rc, Inspector: env.insp, Logf: t.Logf})

	// Sweep #1: persists scheduler B's snapshot while it is alive.
	if err := statsEngine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep #1: %v", err)
	}
	schedB.Shutdown() // clean shutdown unregisters its live entries
	waitForCond(t, "scheduler B entries unregistered", 10*time.Second, func() bool {
		entries, err := env.insp.SchedulerEntries()
		return err == nil && len(entries) == 2
	})

	// ---- Queue fixtures. ----
	// orders: 3 completions retained 2h + 1 retained 48h.
	for i := 0; i < 3; i++ {
		if _, err := client.Enqueue(asynq.NewTask("order:process", nil),
			asynq.Queue("orders"), asynq.Retention(2*time.Hour)); err != nil {
			t.Fatalf("enqueue order: %v", err)
		}
	}
	if _, err := client.Enqueue(asynq.NewTask("order:process", nil),
		asynq.Queue("orders"), asynq.Retention(48*time.Hour)); err != nil {
		t.Fatalf("enqueue long-retention order: %v", err)
	}
	// payments: 3 failures with MaxRetry(0) → archived with a real error.
	for i := 0; i < 3; i++ {
		if _, err := client.Enqueue(asynq.NewTask("pay:capture", []byte(fmt.Sprintf(`{"n":%d}`, i))),
			asynq.Queue("payments"), asynq.MaxRetry(0)); err != nil {
			t.Fatalf("enqueue payment: %v", err)
		}
	}
	// media: fat payloads, nobody consumes → zero-consumer with pending.
	fat := make([]byte, 4096)
	for i := range fat {
		fat[i] = byte('a' + i%26)
	}
	for i := 0; i < 2; i++ {
		if _, err := client.Enqueue(asynq.NewTask("media:transcode", fat), asynq.Queue("media")); err != nil {
			t.Fatalf("enqueue media: %v", err)
		}
	}
	// billing: paused >7d (value backdated below) with one pending task.
	if _, err := client.Enqueue(asynq.NewTask("bill:run", nil), asynq.Queue("billing")); err != nil {
		t.Fatalf("enqueue billing: %v", err)
	}
	if err := env.insp.PauseQueue("billing"); err != nil {
		t.Fatalf("pausing billing: %v", err)
	}
	// ghost: registered queue, zero size, zero consumers, no counters ever.
	if _, err := client.Enqueue(asynq.NewTask("ghost:once", nil), asynq.Queue("ghost")); err != nil {
		t.Fatalf("enqueue ghost: %v", err)
	}
	if _, err := env.insp.DeleteAllPendingTasks("ghost"); err != nil {
		t.Fatalf("emptying ghost: %v", err)
	}

	waitForCond(t, "orders completed and payments archived", 15*time.Second, func() bool {
		qi, err := env.insp.GetQueueInfo("orders")
		if err != nil || qi.Completed != 4 {
			return false
		}
		pi, err := env.insp.GetQueueInfo("payments")
		return err == nil && pi.Archived == 3
	})

	now := time.Now()

	// ---- Documented backdates (scores/values only; members are authentic). --
	// billing paused 8 days ago.
	if err := env.rc.Set(ctx, "asynq:{billing}:paused",
		fmt.Sprintf("%d", now.Add(-8*24*time.Hour).Unix()), 0).Err(); err != nil {
		t.Fatalf("backdating paused marker: %v", err)
	}
	// one 2h-retention completion already past its expiry (janitor lag).
	completed, err := env.insp.ListCompletedTasks("orders", asynq.PageSize(10))
	if err != nil || len(completed) != 4 {
		t.Fatalf("listing completed: %v (n=%d)", err, len(completed))
	}
	backdated := false
	for _, ti := range completed {
		if ti.Retention == 2*time.Hour && !backdated {
			if err := env.rc.ZAdd(ctx, "asynq:{orders}:completed",
				redis.Z{Score: float64(now.Add(-time.Minute).Unix()), Member: ti.ID}).Err(); err != nil {
				t.Fatalf("backdating completed expiry: %v", err)
			}
			backdated = true
		}
	}
	if !backdated {
		t.Fatal("no 2h-retention completed task found to backdate")
	}
	// archived died-at: one 3d old, one 31d old, one stays fresh.
	archived, err := env.insp.ListArchivedTasks("payments", asynq.PageSize(10))
	if err != nil || len(archived) != 3 {
		t.Fatalf("listing archived: %v (n=%d)", err, len(archived))
	}
	if err := env.rc.ZAdd(ctx, "asynq:{payments}:archived",
		redis.Z{Score: float64(now.Add(-3 * 24 * time.Hour).Unix()), Member: archived[0].ID},
		redis.Z{Score: float64(now.Add(-31 * 24 * time.Hour).Unix()), Member: archived[1].ID},
	).Err(); err != nil {
		t.Fatalf("backdating archived died-at: %v", err)
	}

	// ---- Error-signature index feed (real read path for the digest). ----
	indexer := errsig.NewIndexer(errsig.Config{RedisClient: env.rc, Inspector: env.insp, Logf: t.Logf})
	if err := indexer.SweepTailNow(ctx); err != nil {
		t.Fatalf("errsig tail sweep: %v", err)
	}

	// Sweep #2: the snapshot cache the reports read (B stays a corpse).
	if err := statsEngine.SweepNow(ctx); err != nil {
		t.Fatalf("sweep #2: %v", err)
	}

	// ---- Generate all four reports. ----
	engine := hygiene.NewEngine(hygiene.Config{
		RedisClient:        env.rc,
		Inspector:          env.insp,
		Snapshots:          statsEngine.Read,
		SchedulerGoneAfter: 100 * time.Millisecond,
		Logf:               t.Logf,
	})
	time.Sleep(150 * time.Millisecond) // let B's snapshot age past the GONE threshold

	inv, err := engine.Run(ctx, hygiene.KindInventory)
	if err != nil {
		t.Fatalf("inventory run: %v", err)
	}
	sto, err := engine.Run(ctx, hygiene.KindStorage)
	if err != nil {
		t.Fatalf("storage run: %v", err)
	}
	dl, err := engine.Run(ctx, hygiene.KindDeadLetter)
	if err != nil {
		t.Fatalf("dead-letter run: %v", err)
	}
	sh, err := engine.Run(ctx, hygiene.KindSchedulerHealth)
	if err != nil {
		t.Fatalf("scheduler-health run: %v", err)
	}
	persisted, err := hygiene.ReadReport(ctx, env.rc, hygiene.KindInventory)
	if err != nil {
		t.Fatalf("reading persisted report: %v", err)
	}

	invRow := func(q string) *hygiene.InventoryRow {
		for i := range inv.Inventory.Rows {
			if inv.Inventory.Rows[i].Queue == q {
				return &inv.Inventory.Rows[i]
			}
		}
		return nil
	}

	Convey("Given a real seeded fleet and all four generated reports", t, func() {
		Convey("Then the queue inventory detects the dead queue and only it", func() {
			ghost := invRow("ghost")
			So(ghost, ShouldNotBeNil)
			So(ghost.DeadCandidate, ShouldBeTrue)
			So(ghost.DeadEvidence, ShouldEqual, hygiene.ActivityDailyCounters)
			So(ghost.LastActivitySource, ShouldEqual, hygiene.ActivityNone)
			So(inv.Summary["dead_queue_candidates"], ShouldEqual, 1)
		})

		Convey("Then live activity evidence lands on the busy queues", func() {
			So(invRow("orders").DeadCandidate, ShouldBeFalse)
			So(invRow("orders").LastActivitySource, ShouldEqual, hygiene.ActivityCountersToday)
			So(invRow("payments").LastActivitySource, ShouldEqual, hygiene.ActivityCountersToday)
			So(invRow("media").LastActivitySource, ShouldEqual, hygiene.ActivityOldestPending)
		})

		Convey("Then zero-consumer-pending and paused>7d flags land where seeded", func() {
			So(invRow("media").ZeroConsumerPending, ShouldBeTrue)
			So(invRow("billing").PausedOver7d, ShouldBeTrue)
			So(inv.Summary["paused_over_7d"], ShouldEqual, 1)
			So(inv.Summary["zero_consumer_pending"], ShouldEqual, 2) // media + paused billing
		})

		Convey("Then the storage report samples every task-bearing queue and finds the payload offender", func() {
			So(sto.Storage.NotSampledQueues, ShouldEqual, 0)
			var media *hygiene.QueueMemory
			for i := range sto.Storage.Memory {
				if sto.Storage.Memory[i].Queue == "media" {
					media = &sto.Storage.Memory[i]
				}
			}
			So(media, ShouldNotBeNil)
			So(media.SampledTasks, ShouldEqual, 2)
			So(media.AvgTaskBytes, ShouldBeGreaterThan, 4096)
			So(media.EstimatedBytes, ShouldBeGreaterThan, media.StructBytes)
			So(len(sto.Storage.Offenders), ShouldBeGreaterThan, 0)
			So(sto.Storage.Offenders[0].Queue, ShouldEqual, "media")
			So(sto.Storage.Offenders[0].MsgBytes, ShouldBeGreaterThanOrEqualTo, 4096)
			So(sto.Storage.Method, ShouldContainSubstring, "sampled")
		})

		Convey("Then the retention-pressure histogram is exact: 1 past due, 2 in 1-6h, 1 in 1-3d", func() {
			ret := sto.Storage.Retention
			So(ret.BucketLabels, ShouldResemble, []string{"past_due", "<1h", "1-6h", "6-24h", "1-3d", "3-7d", ">7d"})
			So(ret.Queues, ShouldHaveLength, 1)
			So(ret.Queues[0].Queue, ShouldEqual, "orders")
			So(ret.Fleet.Counts[0], ShouldEqual, 1) // past_due (backdated expiry)
			So(ret.Fleet.Counts[2], ShouldEqual, 2) // 1-6h (2h retention)
			So(ret.Fleet.Counts[4], ShouldEqual, 1) // 1-3d (48h retention)
			So(ret.Fleet.Total, ShouldEqual, 4)
			So(sto.Summary["reclaim_past_due"], ShouldEqual, 1)
			So(sto.Summary["reclaim_24h"], ShouldEqual, 3)
		})

		Convey("Then the dead-letter digest ages the archive exactly and attaches the one-click action", func() {
			So(dl.DeadLetter.Queues, ShouldHaveLength, 1)
			q := dl.DeadLetter.Queues[0]
			So(q.Queue, ShouldEqual, "payments")
			So(q.Archived, ShouldEqual, 3)
			So(q.Buckets, ShouldResemble, []int64{1, 1, 0, 1}) // <1d, 1-7d, 7-30d, >30d
			So(q.OlderThanThreshold, ShouldEqual, 1)
			So(q.AtTrimCap, ShouldBeFalse)
			So(q.Action, ShouldNotBeNil)
			So(q.Action.Verb, ShouldEqual, "delete")
			So(q.Action.State, ShouldEqual, "archived")
			So(q.Action.Aql, ShouldEqual, "died>30d")
			So(q.Action.Estimate, ShouldEqual, 1)
			So(dl.Summary["older_than_30d"], ShouldEqual, 1)
		})

		Convey("Then the digest's error clusters come from the errsig read path", func() {
			q := dl.DeadLetter.Queues[0]
			So(len(q.Clusters), ShouldBeGreaterThan, 0)
			So(q.Clusters[0].Template, ShouldContainSubstring, "card declined")
			So(q.Clusters[0].Archived, ShouldEqual, 3)
			So(dl.DeadLetter.ClustersIndexedAt.IsZero(), ShouldBeFalse)
		})

		Convey("Then scheduler health reports the corpse, the untraceable entry and the unconsumed target", func() {
			rep := sh.SchedulerHealth
			So(rep.LiveEntries, ShouldEqual, 2)
			So(rep.Gone, ShouldHaveLength, 1)
			So(rep.Gone[0].TaskType, ShouldEqual, "cron:dead")
			So(rep.Gone[0].LastSeenAt.IsZero(), ShouldBeFalse)
			So(rep.RetentionLess, ShouldHaveLength, 1)
			So(rep.RetentionLess[0].TaskType, ShouldEqual, "cron:noretention")
			So(rep.ZeroConsumerTargets, ShouldHaveLength, 1)
			So(rep.ZeroConsumerTargets[0].TaskType, ShouldEqual, "cron:noretention")
			So(rep.ZeroConsumerTargets[0].Queue, ShouldEqual, "maintenance")
			// cron:traceable (retention set, consumed target) is in no list.
			So(sh.Summary["gone"], ShouldEqual, 1)
		})

		Convey("Then reports persist as latest with honest stamps", func() {
			So(persisted, ShouldNotBeNil)
			So(persisted.GeneratedAt.IsZero(), ShouldBeFalse)
			So(persisted.DataAsOf, ShouldNotBeNil) // the sweep's RefreshedAt
			So(persisted.DataAsOf.IsZero(), ShouldBeFalse)
			So(persisted.Summary["dead_queue_candidates"], ShouldEqual, 1)
			// scheduler-health needs no snapshot: its stamp is honestly absent.
			So(sh.DataAsOf, ShouldBeNil)
		})
	})
}

// discardLogger silences the asynq server's own logging in tests.
type discardLogger struct{}

func (discardLogger) Debug(args ...interface{}) {}
func (discardLogger) Info(args ...interface{})  {}
func (discardLogger) Warn(args ...interface{})  {}
func (discardLogger) Error(args ...interface{}) {}
func (discardLogger) Fatal(args ...interface{}) {}

// hygTestClock is a mutable injected clock.
type hygTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hygTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *hygTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestHygieneSchedulePassAndWebhook(t *testing.T) {
	env := newHygieneTestEnv(t)
	ctx := context.Background()

	// Webhook sink: collects delivered report kinds.
	var whMu sync.Mutex
	var delivered []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rep hygiene.Report
		if json.Unmarshal(body, &rep) == nil {
			whMu.Lock()
			delivered = append(delivered, rep.Kind)
			whMu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.Close)

	clock := &hygTestClock{t: time.Now()}
	engine := hygiene.NewEngine(hygiene.Config{
		RedisClient: env.rc,
		Inspector:   env.insp,
		WebhookURL:  sink.URL,
		Now:         clock.Now,
		Logf:        t.Logf,
	})

	// Enable only scheduler-health, hourly (it needs no stats engine).
	if err := hygiene.WriteConfig(ctx, env.rc, hygiene.KindSchedulerHealth,
		hygiene.KindConfig{Enabled: true, IntervalSeconds: 3600}); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	// Pass 1: due (never generated) → generates + delivers.
	if err := engine.SchedulePass(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	rep1, _ := hygiene.ReadReport(ctx, env.rc, hygiene.KindSchedulerHealth)

	// Pass 2 at +30m: not due → unchanged, no delivery.
	clock.Advance(30 * time.Minute)
	if err := engine.SchedulePass(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	rep2, _ := hygiene.ReadReport(ctx, env.rc, hygiene.KindSchedulerHealth)

	// Pass 3 at +61m: due again → regenerates + delivers.
	clock.Advance(31 * time.Minute)
	if err := engine.SchedulePass(ctx); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	rep3, _ := hygiene.ReadReport(ctx, env.rc, hygiene.KindSchedulerHealth)
	invAfter, _ := hygiene.ReadReport(ctx, env.rc, hygiene.KindInventory)

	whMu.Lock()
	deliveredCopy := append([]string(nil), delivered...)
	whMu.Unlock()

	Convey("Given a leased-scheduler pass cycle with an hourly report and a webhook", t, func() {
		Convey("Then the first pass generates and the interval gates the second", func() {
			So(rep1, ShouldNotBeNil)
			So(rep2.GeneratedAt, ShouldEqual, rep1.GeneratedAt)
			So(rep3.GeneratedAt.After(rep1.GeneratedAt), ShouldBeTrue)
		})

		Convey("Then disabled kinds are never generated", func() {
			So(invAfter, ShouldBeNil)
		})

		Convey("Then the webhook received exactly one POST per generation — no retries, no extras", func() {
			So(deliveredCopy, ShouldResemble, []string{"scheduler-health", "scheduler-health"})
		})
	})
}

func TestHygieneSchedulerLease(t *testing.T) {
	env := newHygieneTestEnv(t)
	ctx := context.Background()

	mk := func(id string) *hygiene.Engine {
		return hygiene.NewEngine(hygiene.Config{
			RedisClient:  env.rc,
			Inspector:    env.insp,
			InstanceID:   id,
			LeaseTTL:     300 * time.Millisecond,
			TickInterval: 50 * time.Millisecond,
			Logf:         t.Logf,
		})
	}
	e1 := mk("hyg-replica-1")
	e2 := mk("hyg-replica-2")

	e1.Start(ctx)
	waitForCond(t, "replica 1 to acquire the lease", 5*time.Second, e1.LeaseHeld)
	e2.Start(ctx)
	t.Cleanup(e2.Stop)

	// Give replica 2 several lease ticks to (wrongly) steal.
	time.Sleep(500 * time.Millisecond)
	oneHolderWhileBothLive := e1.LeaseHeld() && !e2.LeaseHeld()

	// Holder stops → standby takes over promptly (release, not TTL expiry).
	e1.Stop()
	waitForCond(t, "replica 2 to take over the lease", 5*time.Second, e2.LeaseHeld)
	takeover := e2.LeaseHeld()

	Convey("Given two replicas competing for the hygiene scheduler lease", t, func() {
		Convey("Then exactly one holds it while both live", func() {
			So(oneHolderWhileBothLive, ShouldBeTrue)
		})
		Convey("Then a stopping holder hands over to the standby", func() {
			So(takeover, ShouldBeTrue)
		})
	})
}
