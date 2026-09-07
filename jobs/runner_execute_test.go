package jobs

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
// Execute-loop tests for the jobs runner (#40) and the job-artifact TTLs
// (#45.3), against a real Redis on DB 0 — the only DB no other suite uses
// (root handlers use 4 and 7-13, hygiene 5, errsig 6 and 9, stats 14,
// leasefence 15, cmd 1-3). Skipped when Redis does not answer PING.
// ****************************************************************************

const jobsTestRedisDB = 0

var jobsTestRedisAddr = testRedisAddrFromEnv()

func jobsTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB})
	ctx := context.Background()
	if err := rc.Ping(ctx).Err(); err != nil {
		rc.Close()
		t.Skipf("skipping: redis not available on %s: %v", jobsTestRedisAddr, err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		rc.Close()
		t.Fatalf("flushing test db: %v", err)
	}
	t.Cleanup(func() { rc.Close() })
	return rc
}

// seedExecutableJob enqueues n pending tasks in one queue, creates a running
// execute-phase job over them, claims it, and writes the candidate refs.
// It returns the runner, the loaded job, the claim session and the token.
func seedExecutableJob(t *testing.T, rc redis.UniversalClient, insp *asynq.Inspector, client *asynq.Client, qname string, n int) (*Runner, *Job, *claimSession, int64) {
	t.Helper()
	ctx := context.Background()

	refs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		info, err := client.Enqueue(asynq.NewTask("jobs:x", []byte(fmt.Sprintf(`{"i":%d}`, i))), asynq.Queue(qname))
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		refs = append(refs, info.Queue+candidateSep+info.ID)
	}

	r := NewRunner(Config{
		RedisClient: rc,
		Inspector:   insp,
		Logf:        t.Logf,
	})
	j := &Job{
		ID:              NewJobID(),
		Verb:            VerbArchive,
		Scope:           Scope{Queue: qname, State: "pending"},
		Phase:           PhaseExecute,
		State:           StateRunning,
		Throttle:        1000,
		Reason:          "test",
		Actor:           "test",
		Counts:          Counts{Candidates: int64(n)},
		PreviewComplete: true,
		CreatedAt:       time.Now(),
	}
	if err := r.store.Create(ctx, j); err != nil {
		t.Fatalf("creating job: %v", err)
	}
	token, err := r.store.Claim(ctx, j.ID, r.cfg.InstanceID, r.cfg.LeaseTTL)
	if err != nil || token == 0 {
		t.Fatalf("claiming job: token=%d err=%v", token, err)
	}
	ok, err := r.store.WriteProgress(ctx, j.ID, token, ProgressWrite{Candidates: refs})
	if err != nil || !ok {
		t.Fatalf("writing candidates: ok=%t err=%v", ok, err)
	}
	loaded, err := r.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("loading job: %v", err)
	}
	return r, loaded, &claimSession{stop: make(chan struct{})}, token
}

// TestExecuteCountsAlreadyInStateAsSkipped: after a claim handover both the
// stalled holder and its successor can act on the same task; the loser's
// Inspector error says the task is already in the target state, and that
// counts as skipped, never as a failure (#40).
func TestExecuteCountsAlreadyInStateAsSkipped(t *testing.T) {
	rc := jobsTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	r, j, cs, token := seedExecutableJob(t, rc, insp, client, "jq_already", 3)
	// Stand in for an Inspector whose task a successor already archived.
	r.actFn = func(v Verb, qname, taskID string) error {
		return fmt.Errorf("asynq: task is already archived")
	}
	r.execute(ctx, j, token, cs)

	final, err := r.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("loading job: %v", err)
	}

	Convey("Given three tasks a successor already archived", t, func() {
		Convey("When this runner executes the batch", func() {
			Convey("Then every task counts as skipped, none as failed", func() {
				So(final.Counts.Skipped, ShouldEqual, int64(3))
				So(final.Counts.Failed, ShouldEqual, int64(0))
				So(final.Counts.Acted, ShouldEqual, int64(0))
			})
			Convey("Then no per-item failures were recorded", func() {
				_, total, ferr := r.store.Failures(ctx, j.ID, 0, 10)
				So(ferr, ShouldBeNil)
				So(total, ShouldEqual, int64(0))
			})
			Convey("Then the job still finished", func() {
				So(final.State, ShouldEqual, StateDone)
			})
		})
	})
}

// TestExecuteStopsPerTaskWhenClaimLost: the act loop checks the claim per
// task, so a lease lost mid-batch stops at the next task instead of acting
// on the whole batch a successor is already acting on (#40).
func TestExecuteStopsPerTaskWhenClaimLost(t *testing.T) {
	rc := jobsTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	r, j, cs, token := seedExecutableJob(t, rc, insp, client, "jq_lost", 5)
	calls := 0
	r.actFn = func(v Verb, qname, taskID string) error {
		calls++
		if calls == 2 {
			// The renewal goroutine would flip this flag here.
			cs.lost = 1
		}
		return nil
	}
	r.execute(ctx, j, token, cs)

	final, err := r.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatalf("loading job: %v", err)
	}

	Convey("Given a claim lost while a batch of five is acting", t, func() {
		Convey("When the act loop reaches the next task", func() {
			Convey("Then it stops immediately instead of finishing the batch", func() {
				So(calls, ShouldEqual, 2)
			})
			Convey("Then the acts already done are persisted and the cursor did not advance", func() {
				So(final.Counts.Acted, ShouldEqual, int64(2))
				So(final.State, ShouldEqual, StateRunning)
			})
		})
	})
}

// TestExecuteAbortsWhenRenewalFails: the synchronous renewal before each
// batch is the gate; a claim held by another replica stops the execute
// before any task is acted on (#40).
func TestExecuteAbortsWhenRenewalFails(t *testing.T) {
	rc := jobsTestRedis(t)
	ctx := context.Background()
	opt := asynq.RedisClientOpt{Addr: jobsTestRedisAddr, DB: jobsTestRedisDB}
	client := asynq.NewClient(opt)
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { client.Close(); insp.Close() })

	r, j, cs, token := seedExecutableJob(t, rc, insp, client, "jq_renew", 3)
	// A successor takes the lease over before the batch starts.
	if err := rc.Set(ctx, leaseKey(j.ID), "another-replica", time.Minute).Err(); err != nil {
		t.Fatalf("seeding successor lease: %v", err)
	}
	calls := 0
	r.actFn = func(v Verb, qname, taskID string) error { calls++; return nil }
	r.execute(ctx, j, token, cs)

	Convey("Given a claim another replica now holds", t, func() {
		Convey("When the batch starts", func() {
			Convey("Then the synchronous renewal fails and no task is acted on", func() {
				So(calls, ShouldEqual, 0)
				So(cs.isLost(), ShouldBeTrue)
			})
		})
	})
}

// TestIsAlreadyInStateErr is the table test the string match needs (#40):
// asynq has no typed error for "already in that state".
func TestIsAlreadyInStateErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"already archived", errors.New("asynq: task is already archived"), true},
		{"already pending", errors.New("task is already pending"), true},
		{"already completed", errors.New("TASK IS ALREADY COMPLETED"), true},
		{"already active", errors.New("cannot archive task: it is already active"), true},
		{"already retry", errors.New("task is already retry"), true},
		{"already scheduled", errors.New("task is already scheduled"), true},
		{"already aggregating", errors.New("task is already aggregating"), true},
		{"already, but no state", errors.New("connection already closed"), false},
		{"real failure", errors.New("redis: connection refused"), false},
		{"not found", errors.New("asynq: task not found"), false},
	}

	Convey("Given Inspector errors from an act", t, func() {
		for _, tc := range cases {
			tc := tc
			Convey("When the error is "+tc.name, func() {
				Convey(fmt.Sprintf("Then isAlreadyInStateErr reports %t", tc.want), func() {
					So(isAlreadyInStateErr(tc.err), ShouldEqual, tc.want)
				})
			})
		}
	})
}

// TestProgressWriteSetsArtifactTTLs: the candidates, sample and failures
// keys get their TTL inside progressScript, so a runner that dies right
// after the write leaves no TTL-less key behind (#45.3).
func TestProgressWriteSetsArtifactTTLs(t *testing.T) {
	rc := jobsTestRedis(t)
	ctx := context.Background()
	store := NewStore(rc)

	j := &Job{
		ID:        NewJobID(),
		Verb:      VerbArchive,
		Scope:     Scope{Queue: "ttlq", State: "pending"},
		Phase:     PhasePreview,
		State:     StatePreviewing,
		CreatedAt: time.Now(),
	}
	if err := store.Create(ctx, j); err != nil {
		t.Fatalf("creating job: %v", err)
	}
	token, err := store.Claim(ctx, j.ID, "inst-1", time.Minute)
	if err != nil || token == 0 {
		t.Fatalf("claiming job: token=%d err=%v", token, err)
	}
	ok, err := store.WriteProgress(ctx, j.ID, token, ProgressWrite{
		Fields:     map[string]string{"scanned": "1"},
		Candidates: []string{"ttlq" + candidateSep + "task-1"},
		Sample:     []SampleRow{{ID: "task-1", Queue: "ttlq"}},
		Failures:   []ItemFailure{{Queue: "ttlq", ID: "task-1", Error: "boom"}},
	})
	if err != nil || !ok {
		t.Fatalf("writing progress: ok=%t err=%v", ok, err)
	}

	Convey("Given one fence-guarded progress write", t, func() {
		Convey("When the script returns (no trailing EXPIRE pipeline exists any more)", func() {
			Convey("Then all three artifact keys already carry a TTL", func() {
				for _, k := range []string{candidatesKey(j.ID), sampleKey(j.ID), failuresKey(j.ID)} {
					pttl := rc.PTTL(ctx, k).Val()
					So(pttl, ShouldBeGreaterThan, 0)
					So(pttl, ShouldBeLessThanOrEqualTo, jobTTL)
				}
			})
		})
	})
}
