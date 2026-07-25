//go:build scaletest

package stats

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// SCALE VALIDATION HARNESS (opt-in: go test -tags scaletest -run TestScale
// -v ./stats/ -timeout 15m). Validates the §5.1 tiers-2-3 governor against a
// REAL large fleet on Redis DB 14 — real asynq client seeding, the real
// Engine on a real wall clock — not the simulated 100-queue integration
// fixture. Results and re-run instructions: docs/SCALE.md.
//
// Tier 2: 5,000 queues (realistic depth distribution, paused/grouped/
//   scheduled/retry spread), CommandBudget 2000 cmds/s, Interval 1s.
// Tier 3: 25,000 queue names (spot check: hot set every tick, rotation
//   progress, no goroutine/heap blowup over ~60s).
//
// The fixture has no live workers, so every observed queue with pending work
// raises NO_CONSUMERS — deliberately kept: it exercises the mass-incident
// hot-set ceiling (rotation.go) under the worst attention flood.
// ****************************************************************************

const (
	scaleFleetSize     = 5000
	scaleDeepQueues    = 100
	scaleBudget        = 2000
	scaleInterval      = time.Second
	scaleWarmupTimeout = 240 * time.Second
)

// scaleDeepDepth returns the seeded backlog of deep queue i (0..99): the top
// 20 range 2000→1050 and the rest 495→100, all distinct so attention ranking
// and hot-set membership are deterministic.
func scaleDeepDepth(i int) int {
	if i < 20 {
		return 2000 - 50*i
	}
	return 100 + 5*(99-i)
}

// seedTask is one enqueue instruction for the parallel seeder.
type seedTask struct {
	queue string
	typ   string
	opts  []asynq.Option
}

// seedParallel enqueues tasks through the real asynq client with a worker
// pool. The client is safe for concurrent use; 32 workers keep the ~60k
// enqueues to a few seconds against a local Redis.
func seedParallel(t *testing.T, client *asynq.Client, tasks <-chan seedTask) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for st := range tasks {
				if _, err := client.Enqueue(asynq.NewTask(st.typ, nil), append(st.opts, asynq.Queue(st.queue))...); err != nil {
					select {
					case errCh <- fmt.Errorf("enqueue %s: %w", st.queue, err):
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("seeding: %v", err)
	default:
	}
}

// commandCalls sums calls= over INFO commandstats — the server-side truth of
// how many commands Redis executed (including calls made from inside Lua
// scripts, which is exactly what makes it the right cross-check for the
// fenced write path).
func commandCalls(ctx context.Context, t *testing.T, rc redis.UniversalClient) int64 {
	t.Helper()
	info, err := rc.Info(ctx, "commandstats").Result()
	if err != nil {
		t.Fatalf("INFO commandstats: %v", err)
	}
	var total int64
	for _, line := range strings.Split(info, "\n") {
		idx := strings.Index(line, "calls=")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("calls="):]
		if end := strings.IndexByte(rest, ','); end > 0 {
			rest = rest[:end]
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
			total += n
		}
	}
	return total
}

// memUsage returns MEMORY USAGE for a key (0 if the key is missing).
func memUsage(ctx context.Context, rc redis.UniversalClient, key string) int64 {
	n, err := rc.MemoryUsage(ctx, key).Result()
	if err != nil {
		return 0
	}
	return n
}

// scaleCollector subscribes to sweep completions and accumulates per-tick
// measurements. All engine reads are in-process (SourceLocal) — the
// collector adds zero Redis traffic, keeping the commandstats window clean.
type scaleCollector struct {
	eng     *Engine
	targets []string

	mu           sync.Mutex
	observed     int // len of last Read (fleet coverage so far)
	lastAt       time.Time
	windowActive bool
	ticks        int
	perTick      []int // ReadCmds+WriteCmds per steady-window sweep
	perTickR     []int
	perTickW     []int
	hotSizes     []int
	// Always-on anomaly detectors (all sweeps, not just the window): the
	// smallest universe any sweep saw (a transient empty/partial
	// asynq:queues read would flush every series accumulator) and the tier
	// of every sweep since start.
	minUniverse int
	allTiers    map[int]int
	tiers       map[int]int
	advanced    map[string]int
	prevStamp   map[string]time.Time
	durations   []time.Duration
}

func newScaleCollector(eng *Engine, targets []string) *scaleCollector {
	return &scaleCollector{
		eng:         eng,
		targets:     targets,
		tiers:       map[int]int{},
		advanced:    map[string]int{},
		prevStamp:   map[string]time.Time{},
		minUniverse: 1 << 30,
		allTiers:    map[int]int{},
	}
}

func (c *scaleCollector) run(ctx context.Context, done <-chan struct{}) {
	ch, cancel := c.eng.SubscribeSweeps()
	defer cancel()
	for {
		select {
		case <-done:
			return
		case <-ch:
		}
		st := c.eng.LastSweep()
		res, err := c.eng.Read(ctx)
		if err != nil {
			continue
		}
		stamps := make(map[string]time.Time, len(c.targets))
		for _, s := range res.Queues {
			stamps[s.Queue] = s.RefreshedAt
		}
		c.mu.Lock()
		c.observed = len(res.Queues)
		if !st.At.Equal(c.lastAt) {
			c.lastAt = st.At
			if st.Queues < c.minUniverse {
				c.minUniverse = st.Queues
			}
			c.allTiers[st.Tier]++
			if c.windowActive {
				c.ticks++
				c.perTick = append(c.perTick, st.ReadCmds+st.WriteCmds)
				c.perTickR = append(c.perTickR, st.ReadCmds)
				c.perTickW = append(c.perTickW, st.WriteCmds)
				c.hotSizes = append(c.hotSizes, st.HotSetSize)
				c.tiers[st.Tier]++
				c.durations = append(c.durations, st.Duration)
				for _, q := range c.targets {
					if at, ok := stamps[q]; ok && at.After(c.prevStamp[q]) {
						c.advanced[q]++
					}
					c.prevStamp[q] = stamps[q]
				}
			}
		}
		c.mu.Unlock()
	}
}

func (c *scaleCollector) observedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observed
}

func (c *scaleCollector) setWindow(active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windowActive = active
}

// snapshot returns the collected window measurements.
func (c *scaleCollector) snapshot() (ticks int, perTick, perTickR, perTickW, hotSizes []int, tiers map[int]int, advanced map[string]int, durations []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pt := append([]int(nil), c.perTick...)
	ptr := append([]int(nil), c.perTickR...)
	ptw := append([]int(nil), c.perTickW...)
	hs := append([]int(nil), c.hotSizes...)
	tr := map[int]int{}
	for k, v := range c.tiers {
		tr[k] = v
	}
	ad := map[string]int{}
	for k, v := range c.advanced {
		ad[k] = v
	}
	du := append([]time.Duration(nil), c.durations...)
	return c.ticks, pt, ptr, ptw, hs, tr, ad, du
}

// independentRefreshedPct reads every cached row's refreshed_at RAW from
// Redis (bypassing the engine's read path entirely) and returns the fraction
// younger than the coverage window — the ground truth the coverage stamp's
// refreshed_pct_5m must agree with.
func independentRefreshedPct(ctx context.Context, t *testing.T, rc redis.UniversalClient, now time.Time, window time.Duration) (pct float64, rows int) {
	t.Helper()
	names, err := rc.SMembers(ctx, queueIndexKey).Result()
	if err != nil {
		t.Fatalf("SMEMBERS cache index: %v", err)
	}
	fresh := 0
	for start := 0; start < len(names); start += 500 {
		end := start + 500
		if end > len(names) {
			end = len(names)
		}
		pipe := rc.Pipeline()
		cmds := make([]*redis.StringCmd, end-start)
		for i, q := range names[start:end] {
			cmds[i] = pipe.HGet(ctx, queueCacheKey(q), "refreshed_at")
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			t.Fatalf("HGET refreshed_at batch: %v", err)
		}
		for _, cmd := range cmds {
			v, err := cmd.Result()
			if err != nil {
				continue
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				if now.Sub(time.Unix(0, n)) <= window {
					fresh++
				}
			}
			rows++
		}
	}
	if rows == 0 {
		return 0, 0
	}
	return float64(fresh) / float64(rows) * 100, rows
}

// TestScaleTier2Fleet5000 is the tier-2 validation: 5,000 real queues under
// CommandBudget=2000, Interval=1s, asserting the §5.1 contract over real
// wall clock:
//
//	(a) the engine classifies and reports tier 2
//	(b) every queue's cache row refreshes within full_rotation_seconds_est × 1.5
//	(c) the hot set (deep backlogs + viewed queues) refreshes every tick
//	(d) measured per-tick command spend ≤ budget × interval × 1.2 — both the
//	    engine's own accounting per tick and the server-side commandstats mean
//	(e) refreshed_pct_5m is consistent with the raw refreshed_at distribution
//	(f) the asynqmon:cache / series memory footprint is measured and logged
func TestScaleTier2Fleet5000(t *testing.T) {
	rc := testRedis(t) // flushes DB 14, skips when Redis is down
	client, insp := testClientInspector(t)
	ctx := context.Background()
	t.Cleanup(func() { rc.FlushDB(context.Background()) })

	// --- Fixture: 5,000 queues through the real asynq client. -------------
	// 4,883 singles + 100 deep + 10 scheduled + 5 grouped + 2 retry = 5,000.
	seedStart := time.Now()
	tasks := make(chan seedTask, 4096)
	go func() {
		defer close(tasks)
		for i := 0; i < scaleFleetSize-scaleDeepQueues-10-5-2; i++ {
			tasks <- seedTask{queue: fmt.Sprintf("sq%04d", i), typ: "scale:one"}
		}
		for i := 0; i < scaleDeepQueues; i++ {
			q := fmt.Sprintf("dq%03d", i)
			for j := 0; j < scaleDeepDepth(i); j++ {
				tasks <- seedTask{queue: q, typ: "scale:deep"}
			}
		}
		// Scheduled spread: 5 far-future + 1 soon-past-due per queue (no
		// server runs, so the forwarder never moves it — a real PAST_DUE).
		for i := 0; i < 10; i++ {
			q := fmt.Sprintf("xq%02d", i)
			for j := 0; j < 5; j++ {
				tasks <- seedTask{queue: q, typ: "scale:later", opts: []asynq.Option{asynq.ProcessIn(2 * time.Hour)}}
			}
			tasks <- seedTask{queue: q, typ: "scale:stall", opts: []asynq.Option{asynq.ProcessIn(500 * time.Millisecond)}}
		}
		// Groups: 2 aggregation groups × 2 tasks each.
		for i := 0; i < 5; i++ {
			q := fmt.Sprintf("gq%02d", i)
			for j := 0; j < 4; j++ {
				tasks <- seedTask{queue: q, typ: "scale:grouped", opts: []asynq.Option{asynq.Group(fmt.Sprintf("g%d", j%2))}}
			}
		}
	}()
	seedParallel(t, client, tasks)

	// Retry spread through asynq's own machinery: a real server fails the
	// tasks; RetryDelayFunc pushes the retries 2h out so they stay in retry
	// state (and outside the RETRY_STORM window) for the whole run.
	srv := asynq.NewServer(testAsynqOpt(), asynq.Config{
		Concurrency:    4,
		Queues:         map[string]int{"rq0": 1, "rq1": 1},
		LogLevel:       asynq.FatalLevel,
		RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return 2 * time.Hour },
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc("scale:fail", func(context.Context, *asynq.Task) error { return fmt.Errorf("boom") })
	if err := srv.Start(mux); err != nil {
		t.Fatalf("starting retry-seed server: %v", err)
	}
	for i := 0; i < 25; i++ {
		for _, q := range []string{"rq0", "rq1"} {
			if _, err := client.Enqueue(asynq.NewTask("scale:fail", nil), asynq.Queue(q)); err != nil {
				t.Fatalf("enqueue retry seed: %v", err)
			}
		}
	}
	waitFor(t, 60*time.Second, "retry state seeded", func() bool {
		for _, q := range []string{"rq0", "rq1"} {
			qi, err := insp.GetQueueInfo(q)
			if err != nil || qi.Retry != 25 {
				return false
			}
		}
		return true
	})
	srv.Shutdown()

	// Paused queues (real asynq pause markers).
	for i := 0; i < 8; i++ {
		if err := insp.PauseQueue(fmt.Sprintf("sq%04d", i)); err != nil {
			t.Fatalf("pause: %v", err)
		}
	}
	fleetN, err := rc.SCard(ctx, allQueuesKey).Result()
	if err != nil || fleetN != scaleFleetSize {
		t.Fatalf("fleet size = %d (err=%v), want %d", fleetN, err, scaleFleetSize)
	}
	t.Logf("seeded %d queues in %s", fleetN, time.Since(seedStart).Round(time.Second))

	// --- The real engine on a real clock, on its own Redis client so the
	// test's verification traffic never pollutes its connection pool. ------
	//
	// The clock advances in real time but is SHIFTED so the measurement
	// window always straddles a UTC hour boundary: the hourly cold-rollup
	// flush hump (docs/SCALE.md, bug 4 — every cold queue's rollup slot
	// completes at the hour and flushes on its next rotation visit) must be
	// inside every validated window, not a timing lottery. The shift places
	// the boundary ~150s after engine start: past warmup (~120s), inside
	// the ~179s window.
	base := time.Now()
	clockShift := base.Truncate(time.Hour).Add(time.Hour).Sub(base) - 150*time.Second
	if clockShift < 0 {
		clockShift += time.Hour
	}
	scaleNow := func() time.Time { return time.Now().Add(clockShift) }
	t.Logf("engine clock shifted %s forward (hour boundary lands mid-window)", clockShift.Round(time.Second))

	rcEngine := redis.NewClient(&redis.Options{Addr: testRedisAddr, DB: testRedisDB})
	t.Cleanup(func() { rcEngine.Close() })
	eng := NewEngine(Config{
		RedisClient:   rcEngine,
		Inspector:     insp,
		Interval:      scaleInterval,
		CommandBudget: scaleBudget,
		Now:           scaleNow,
		Logf:          silentLogf, // rotation chatter would swamp -v output
	})
	tracker := NewViewTracker(rcEngine, scaleNow)
	viewed := []string{"sq0100", "sq0101", "sq0102", "sq0103", "sq0104"}
	for _, q := range viewed {
		tracker.MarkViewed(ctx, q)
	}

	// Hot-set targets: the 10 deepest backlogs (attention-ranked AND
	// top-scored — hot through both §5.1 signals) plus the 5 viewed queues.
	targets := append([]string{}, viewed...)
	for i := 0; i < 10; i++ {
		targets = append(targets, fmt.Sprintf("dq%03d", i))
	}
	coll := newScaleCollector(eng, targets)
	done := make(chan struct{})
	go coll.run(ctx, done)

	runCtx, cancelRun := context.WithCancel(ctx)
	eng.Start(runCtx)
	stopped := false
	stopEngine := func() {
		if !stopped {
			stopped = true
			close(done)
			eng.Stop()
			cancelRun()
		}
	}
	t.Cleanup(stopEngine)

	// Keep the viewed queues inside the 60s viewed window for the whole run.
	viewerDone := make(chan struct{})
	defer close(viewerDone)
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-viewerDone:
				return
			case <-tick.C:
				for _, q := range viewed {
					tracker.MarkViewed(ctx, q)
				}
			}
		}
	}()

	// --- Warmup: unseen-first rotation must discover the whole fleet. -----
	warmStart := time.Now()
	waitFor(t, scaleWarmupTimeout, "whole fleet observed", func() bool {
		return coll.observedCount() >= scaleFleetSize
	})
	warmupDur := time.Since(warmStart)

	steady, err := eng.Read(ctx)
	if err != nil {
		t.Fatalf("steady read: %v", err)
	}
	est := steady.Fleet.FullRotationEstSec
	if est <= 0 || est > 200 {
		t.Fatalf("full_rotation_seconds_est = %d, want (0, 200] — rotation math is off", est)
	}
	t.Logf("fleet observed in %s; tier=%d hot=%d est=%ds",
		warmupDur.Round(time.Second), steady.Fleet.Tier, steady.Fleet.HotSetSize, est)

	// Settle a few ticks so the attention debounce for the last-discovered
	// queues has matured and the hot set is in steady state.
	time.Sleep(5 * scaleInterval)

	// --- Measurement window: one engine-estimated rotation × 1.5. ---------
	window := time.Duration(est) * time.Second * 3 / 2
	callsBefore := commandCalls(ctx, t, rc)
	winStart := time.Now()
	coll.setWindow(true)
	time.Sleep(window)
	coll.setWindow(false)
	callsAfter := commandCalls(ctx, t, rc)
	winDur := time.Since(winStart)

	ticks, perTick, perTickR, perTickW, hotSizes, tiers, advanced, durations := coll.snapshot()
	endNow := scaleNow() // ages compare against the engine's (shifted) clock
	final, err := eng.Read(ctx)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}

	// Engine-side coverage pct (what buildCoverage serves) vs ground truth.
	engineFresh := 0
	var maxAge time.Duration
	for _, s := range final.Queues {
		age := endNow.Sub(s.RefreshedAt)
		if age > maxAge {
			maxAge = age
		}
		if age <= 5*time.Minute {
			engineFresh++
		}
	}
	enginePct := float64(engineFresh) / float64(len(final.Queues)) * 100
	rawPct, rawRows := independentRefreshedPct(ctx, t, rc, scaleNow(), 5*time.Minute)

	// Per-tick spend stats.
	sumCmds, maxCmds := 0, 0
	for _, n := range perTick {
		sumCmds += n
		if n > maxCmds {
			maxCmds = n
		}
	}
	meanCmds := 0.0
	if len(perTick) > 0 {
		meanCmds = float64(sumCmds) / float64(len(perTick))
	}
	serverMeanPerTick := float64(callsAfter-callsBefore) / float64(max(ticks, 1))
	var maxSweepDur time.Duration
	for _, d := range durations {
		if d > maxSweepDur {
			maxSweepDur = d
		}
	}
	minHot, maxHotSeen := 1<<30, 0
	for _, h := range hotSizes {
		if h < minHot {
			minHot = h
		}
		if h > maxHotSeen {
			maxHotSeen = h
		}
	}

	// Memory footprint (logged, not asserted — single local Redis).
	cacheRows, _ := rc.SCard(ctx, queueIndexKey).Result()
	var rowSample, rowSampled int64
	for i := 0; i < 200; i += 1 {
		if n := memUsage(ctx, rc, queueCacheKey(fmt.Sprintf("sq%04d", 200+i))); n > 0 {
			rowSample += n
			rowSampled++
		}
	}
	avgRow := int64(0)
	if rowSampled > 0 {
		avgRow = rowSample / rowSampled
	}
	cacheEst := avgRow*cacheRows + memUsage(ctx, rc, fleetCacheKey) +
		memUsage(ctx, rc, queueIndexKey) + memUsage(ctx, rc, attentionCacheKey)
	seriesKeys, seriesBytes := 0, int64(0)
	hotK, rollK, fleetK, otherK := 0, 0, 0, 0
	hotQueues, rollQueues := map[string]bool{}, map[string]bool{}
	var cursor uint64
	for {
		keys, next, err := rc.Scan(ctx, cursor, "asynqmon:series:*", 2000).Result()
		if err != nil {
			break
		}
		for i, k := range keys {
			seriesKeys++
			if i < 5 { // sample a few per page
				seriesBytes += memUsage(ctx, rc, k)
			}
			switch {
			case strings.HasPrefix(k, "asynqmon:series:h:q:"):
				hotK++
				rest := strings.TrimPrefix(k, "asynqmon:series:h:q:")
				if j := strings.LastIndexByte(rest, ':'); j > 0 {
					hotQueues[rest[:j]] = true
				}
			case strings.HasPrefix(k, "asynqmon:series:r:q:"):
				rollK++
				rest := strings.TrimPrefix(k, "asynqmon:series:r:q:")
				if j := strings.LastIndexByte(rest, ':'); j > 0 {
					rollQueues[rest[:j]] = true
				}
			case strings.Contains(k, ":fleet:"):
				fleetK++
			default:
				otherK++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	budgetPerTick := scaleBudget * int(scaleInterval/time.Second)
	allowance := budgetPerTick * 12 / 10

	sumR, sumW, maxR, maxW := 0, 0, 0, 0
	for i := range perTickR {
		sumR += perTickR[i]
		sumW += perTickW[i]
		if perTickR[i] > maxR {
			maxR = perTickR[i]
		}
		if perTickW[i] > maxW {
			maxW = perTickW[i]
		}
	}
	t.Logf("MEASURED: window=%s ticks=%d | cmds/tick mean=%.0f max=%d server-mean=%.0f allowance=%d | reads mean=%d max=%d writes mean=%d max=%d | hot=[%d..%d] | maxAge=%s est=%ds | pct engine=%.2f raw=%.2f (%d rows) | cacheEst=%dB rows=%d avgRow=%dB | maxSweepDur=%s",
		winDur.Round(time.Second), ticks, meanCmds, maxCmds, serverMeanPerTick, allowance,
		sumR/max(ticks, 1), maxR, sumW/max(ticks, 1), maxW,
		minHot, maxHotSeen, maxAge.Round(time.Second), est, enginePct, rawPct, rawRows,
		cacheEst, cacheRows, avgRow, maxSweepDur.Round(time.Millisecond))
	coll.mu.Lock()
	minUni, allTiers := coll.minUniverse, fmt.Sprintf("%v", coll.allTiers)
	coll.mu.Unlock()
	t.Logf("SERIES CENSUS: total=%d hotRingQueueKeys=%d (%d distinct queues) rollupQueueKeys=%d (%d distinct queues) fleet=%d other=%d | minUniverse=%d allTiers=%s",
		seriesKeys, hotK, len(hotQueues), rollK, len(rollQueues), fleetK, otherK, minUni, allTiers)

	Convey("Given 5,000 real queues under budget 2000 cmds/s at a 1s interval", t, func() {
		Convey("(a) Then the engine runs and reports tier 2", func() {
			So(final.Fleet.Tier, ShouldEqual, 2)
			So(tiers[2], ShouldEqual, ticks) // every window tick swept in tier 2
			So(final.Fleet.CommandBudget, ShouldEqual, scaleBudget)
		})

		Convey("(b) Then every queue refreshed within the engine's own estimate × 1.5", func() {
			So(len(final.Queues), ShouldEqual, scaleFleetSize)
			// +2 intervals of slack for tick quantization at the window edge.
			So(maxAge, ShouldBeLessThanOrEqualTo, time.Duration(est)*time.Second*3/2+2*scaleInterval)
		})

		Convey("(c) Then the hot set refreshed on every tick of the window", func() {
			So(ticks, ShouldBeGreaterThanOrEqualTo, 60) // several minutes of 1s ticks
			for _, q := range targets {
				// -1 tolerates the tick straddling the window boundary.
				So(advanced[q], ShouldBeGreaterThanOrEqualTo, ticks-1)
			}
		})

		Convey("(d) Then per-tick command spend honors budget × interval × 1.2", func() {
			So(len(perTick), ShouldEqual, ticks)
			So(maxCmds, ShouldBeLessThanOrEqualTo, allowance) // engine accounting, EVERY tick
			So(serverMeanPerTick, ShouldBeLessThanOrEqualTo, float64(allowance))
			// Cross-check: self-accounting agrees with the server to within
			// the uncounted traffic (Servers()/SchedulerEntries, lease, Lua
			// chunk overhead, viewed republish).
			So(serverMeanPerTick-meanCmds, ShouldBeLessThan, 100)
			So(serverMeanPerTick-meanCmds, ShouldBeGreaterThan, -100)
		})

		Convey("(e) Then refreshed_pct_5m is consistent with the raw refreshed_at truth", func() {
			So(rawRows, ShouldEqual, scaleFleetSize)
			So(enginePct, ShouldBeGreaterThanOrEqualTo, 99.0)
			So(rawPct, ShouldBeGreaterThanOrEqualTo, 99.0)
			So(rawPct-enginePct, ShouldBeLessThan, 1.0)
			So(enginePct-rawPct, ShouldBeLessThan, 1.0)
		})

		Convey("(f) And the cache footprint was measured and logged above", func() {
			So(cacheEst, ShouldBeGreaterThan, 0)
			So(cacheRows, ShouldEqual, scaleFleetSize)
		})

		Convey("And the hot set stayed inside its budget ceiling all window", func() {
			// refreshBudget/2 at cost 18 = 41 (rotation.go hot-set ceiling).
			So(maxHotSeen, ShouldBeLessThanOrEqualTo, 41)
			So(minHot, ShouldBeGreaterThanOrEqualTo, len(targets))
		})
	})

	stopEngine()
}

// TestScaleTier3SpotCheck25000 drives the fleet past the tier-3 boundary
// with 25,000 queue names and spot-checks 60s of real running: hot set every
// tick, rotation progress, and no goroutine/heap blowup.
func TestScaleTier3SpotCheck25000(t *testing.T) {
	rc := testRedis(t)
	client, insp := testClientInspector(t)
	ctx := context.Background()
	t.Cleanup(func() { rc.FlushDB(context.Background()) })

	// 200 real queues through the asynq client: 20 deep ("adeep…" sorts
	// first, so the unseen-first rotation observes them on tick 1 and they
	// enter the scored/attention hot set immediately) + 180 singles.
	tasks := make(chan seedTask, 1024)
	go func() {
		defer close(tasks)
		for i := 0; i < 20; i++ {
			q := fmt.Sprintf("adeep%02d", i)
			for j := 0; j < 60-i; j++ { // 60..41 tasks, distinct depths
				tasks <- seedTask{queue: q, typ: "scale:deep"}
			}
		}
		for i := 0; i < 180; i++ {
			tasks <- seedTask{queue: fmt.Sprintf("areal%03d", i), typ: "scale:one"}
		}
	}()
	seedParallel(t, client, tasks)

	// DOCUMENTED EXCEPTION (mirrors engine_test.go's expired-lease note):
	// the remaining 24,800 tier-3 queue NAMES are added straight to
	// asynq:queues via SADD instead of the asynq client. Enqueuing 24,800
	// real tasks adds minutes of setup for zero extra coverage — the sweep's
	// per-queue reads (LLEN/ZCARD/GET on missing keys) are exactly the
	// GetQueueInfo-equivalent degrade path, which asynq itself produces for
	// any queue whose tasks have all been processed and trimmed. Everything
	// else in this fixture goes through the real client.
	for start := 0; start < 24800; start += 2000 {
		end := start + 2000
		if end > 24800 {
			end = 24800
		}
		members := make([]interface{}, 0, end-start)
		for i := start; i < end; i++ {
			members = append(members, fmt.Sprintf("cold%05d", i))
		}
		if err := rc.SAdd(ctx, allQueuesKey, members...).Err(); err != nil {
			t.Fatalf("SADD name set: %v", err)
		}
	}
	fleetN, err := rc.SCard(ctx, allQueuesKey).Result()
	if err != nil || fleetN != 25000 {
		t.Fatalf("fleet size = %d (err=%v), want 25000", fleetN, err)
	}

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	g0 := runtime.NumGoroutine()

	rcEngine := redis.NewClient(&redis.Options{Addr: testRedisAddr, DB: testRedisDB})
	t.Cleanup(func() { rcEngine.Close() })
	eng := NewEngine(Config{
		RedisClient:   rcEngine,
		Inspector:     insp,
		Interval:      scaleInterval,
		CommandBudget: scaleBudget,
		Logf:          silentLogf,
	})
	tracker := NewViewTracker(rcEngine, nil)
	viewed := []string{"areal000", "areal001", "areal002"}
	for _, q := range viewed {
		tracker.MarkViewed(ctx, q)
	}
	targets := append([]string{"adeep00", "adeep01", "adeep02", "adeep03", "adeep04"}, viewed...)

	coll := newScaleCollector(eng, targets)
	done := make(chan struct{})
	go coll.run(ctx, done)
	runCtx, cancelRun := context.WithCancel(ctx)
	eng.Start(runCtx)
	stopped := false
	stopEngine := func() {
		if !stopped {
			stopped = true
			close(done)
			eng.Stop()
			cancelRun()
		}
	}
	t.Cleanup(stopEngine)

	// Let the first sweeps land (targets observed), then measure 60 ticks.
	waitFor(t, 30*time.Second, "first sweeps observed the head of the fleet", func() bool {
		return coll.observedCount() >= 300
	})
	observedAtStart := coll.observedCount()
	tracker.MarkViewed(ctx, viewed[0]) // keep window fresh mid-run
	coll.setWindow(true)
	progress := []int{}
	for i := 0; i < 60; i++ {
		time.Sleep(scaleInterval)
		progress = append(progress, coll.observedCount())
		if i == 30 {
			for _, q := range viewed {
				tracker.MarkViewed(ctx, q)
			}
		}
	}
	coll.setWindow(false)
	ticks, perTick, _, _, _, tiers, advanced, _ := coll.snapshot()
	observedAtEnd := coll.observedCount()
	res, err := eng.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	est := res.Fleet.FullRotationEstSec
	stopEngine()

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	g1 := runtime.NumGoroutine()
	maxCmds := 0
	for _, n := range perTick {
		if n > maxCmds {
			maxCmds = n
		}
	}
	heapGrowth := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)

	t.Logf("MEASURED tier3: ticks=%d observed %d→%d est=%ds maxCmds/tick=%d goroutines %d→%d heap %dKB→%dKB (Δ%dKB)",
		ticks, observedAtStart, observedAtEnd, est, maxCmds, g0, g1,
		m0.HeapAlloc/1024, m1.HeapAlloc/1024, heapGrowth/1024)

	Convey("Given 25,000 queue names under budget 2000 cmds/s at a 1s interval", t, func() {
		Convey("Then the engine reports tier 3 for every sweep", func() {
			So(res.Fleet.Tier, ShouldEqual, 3)
			So(tiers[3], ShouldEqual, ticks)
			So(ticks, ShouldBeGreaterThanOrEqualTo, 50)
		})

		Convey("Then the hot set (deep + viewed) refreshed every tick", func() {
			for _, q := range targets {
				So(advanced[q], ShouldBeGreaterThanOrEqualTo, ticks-1)
			}
		})

		Convey("Then the rotation progresses monotonically through the cold fleet", func() {
			So(est, ShouldBeGreaterThan, 0)
			last := observedAtStart
			for _, n := range progress {
				So(n, ShouldBeGreaterThanOrEqualTo, last)
				last = n
			}
			So(observedAtEnd-observedAtStart, ShouldBeGreaterThanOrEqualTo, 1000)
		})

		Convey("Then the per-tick spend still honors the budget in tier 3", func() {
			So(maxCmds, ShouldBeLessThanOrEqualTo, scaleBudget*12/10)
		})

		Convey("Then there is no goroutine or heap blowup", func() {
			So(g1, ShouldBeLessThanOrEqualTo, g0+8)
			So(heapGrowth, ShouldBeLessThan, int64(256<<20))
		})
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
