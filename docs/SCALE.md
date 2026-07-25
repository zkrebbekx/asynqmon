# Stats Engine Scale Validation (tiers 2–3)

Phase-12 introduced the §5.1 command-budget governor: above 2,000 queues the
sweep stops reading every queue every tick and instead refreshes a **hot
set** every tick plus a **rotating cold shard** sized so each tick spends at
most `CommandBudget × Interval` Redis commands. Until now that behavior was
validated only by a simulated 100-queue fixture with tier-ceiling overrides
(`stats/rotation_integration_test.go`).

This document records the validation of tiers 2–3 against a **real fleet** —
5,000 and 25,000 queues seeded through the real asynq client, swept by the
real `Engine` on a real wall clock — and the engine bugs that validation
found and fixed.

## Harness

`stats/scale_test.go`, opt-in behind a build tag so it never runs in normal
suites:

```
go test -tags scaletest -run TestScale -v ./stats/ -timeout 15m
```

Requirements: a local Redis on `127.0.0.1:6379`; **DB 14 is flushed** (it is
the stats package's reserved test database). The harness seeds, runs several
minutes of wall clock, verifies, and flushes DB 14 again on cleanup.

### Tier-2 fixture (`TestScaleTier2Fleet5000`)

5,000 queues (~59,400 tasks, seeded in 4 s), all through the real asynq
client (`Enqueue`, `PauseQueue`, a real `asynq.Server` failing tasks to
produce retry state):

| Component | Count | Detail |
|---|---|---|
| Single-task queues | 4,883 | 1 pending task each |
| Deep queues | 100 | 100–2,000 pending (distinct depths; ~54k tasks total) |
| Scheduled spread | 10 | 5 far-future + 1 past-due `ProcessIn` each |
| Grouped | 5 | 2 aggregation groups × 2 tasks each |
| Retry spread | 2 | 25 tasks failed by a real worker, retries 2h out |
| Paused | 8 | real `PauseQueue` markers |
| Viewed | 5 | marked via the real `ViewTracker` path every 20s |

No live workers run during measurement, so every observed queue with pending
work raises `NO_CONSUMERS` — deliberately kept, because it exercises the
governor under a fleet-wide attention flood (the worst case for the hot set).

Engine config: `CommandBudget=2000` cmds/s, `Interval=1s`, everything else
default. The engine runs `Start()` with the real lease loop. The engine
clock advances in real time but is **shifted so a UTC hour boundary always
lands mid-window** — the hourly cold-rollup flush hump (bug 4 below) is part
of every validated window, not a timing lottery.

Instrumentation:

- **Per-tick spend** — the engine's own `SweepStats` accounting
  (`ReadCmds+WriteCmds` per sweep), captured via `SubscribeSweeps`,
  cross-checked against server-side `INFO commandstats` call deltas over the
  measurement window (which count commands executed inside Lua too, i.e. the
  fenced write path).
- **Coverage** — `refreshed_pct_5m` computed two independent ways: from the
  engine's served snapshots and from raw `HGET refreshed_at` reads of every
  cache row, bypassing the engine entirely.
- **Memory** — `MEMORY USAGE` sampling over the `asynqmon:cache:*` rows and
  a census of `asynqmon:series:*` keys by ring and scope.

### Tier-3 fixture (`TestScaleTier3SpotCheck25000`)

25,000 queue names: 200 real queues via the asynq client (20 deep + 180
singles + 3 viewed), plus 24,800 names added with raw `SADD asynq:queues` —
the harness's one documented seeding exception (enqueuing 24,800 real tasks
adds minutes of setup for zero coverage; the sweep's missing-key reads are
exactly the degrade path asynq produces for fully-drained queues). 60 ticks
measured; goroutine and heap counts taken before start and after stop.

## Measured results

Single local Redis 8.0.1 (Apple Silicon), single asynqmon replica.
Allowance per the validation contract: `budget × interval × 1.2 = 2,400`
commands/tick.

| Metric | Tier 2 (5,000 queues) | Tier 3 (25,000 names) |
|---|---|---|
| Reported tier | 2 (all 302 sweeps) | 3 (all measured sweeps) |
| Fleet observed from cold start | 1 m 58 s (118 ticks, unseen-first) | 335 rows by tick ~5 |
| Hot-set size | 41, stable every tick (= budget ceiling) | ≤ 41 (5 deep + 3 viewed asserted every tick) |
| `full_rotation_seconds_est` | 119 s | 595 s |
| Measured max row age at window end | 1 m 59 s (≤ est × 1.5 = 178 s) | — (spot check) |
| cmds/tick — engine mean | **1,931** (hour-boundary window) / 1,638 (clean window) | — |
| cmds/tick — engine max | **2,006** / 1,873 (clean) — pre-fix: 2,727 ✗ | 1,956 |
| cmds/tick — server mean (commandstats) | 1,937 (Δ engine +6) | — |
| reads/tick mean·max | 1,548 · 1,586 | — |
| writes/tick mean·max | 382 · 420 (flat-cap series drain visible) | — |
| `refreshed_pct_5m` engine / raw | 100.00 / 100.00 (5,000 rows) | — |
| Cache footprint | 392 B/row × 5,000 ≈ **2.23 MB** (rows + fleet + index + attention) | — |
| Series keys | 297 clean window; 18,409 mid-hourly-hump (wave draining, bounded) | — |
| Max sweep duration | 95 ms | — |
| Rotation progress | every queue refreshed each 119 s window | 335 → 2,855 rows over 60 ticks, strictly monotone |
| Goroutines before → after | — | 2 → 2 |
| Heap growth over run | — | +5.0 MB |

Assertion outcomes (all passing after the fixes below):

- **(a)** every sweep reported tier 2; the published fleet hash carries
  `tier=2`, `command_budget=2000`.
- **(b)** at window end every one of the 5,000 rows had
  `age ≤ full_rotation_seconds_est × 1.5` (+2 s tick-quantization slack);
  measured max age 119 s against the 178 s bound.
- **(c)** the 10 deepest queues and the 5 viewed queues refreshed on every
  tick of the 179-tick window (hot via both §5.1 signals).
- **(d)** engine per-tick spend ≤ 2,400 on **every** window tick — including
  the ticks of the UTC-hour rollup hump — and the commandstats mean agreed
  with the engine's self-accounting to within 6 cmds/tick.
- **(e)** `refreshed_pct_5m` from raw `refreshed_at` values matched the
  engine-served value exactly (both 100.00 after one rotation).
- **(f)** cache/series footprint measured and logged (no assertion — single
  local node).
- Tier 3: hot set every tick, monotone rotation progress, per-tick max
  1,956 ≤ 2,400, no goroutine leak, heap growth 5 MB over 60 s.

## Bugs found and fixed

Validation was designed to fail loudly; it did. All four are engine bugs
that only manifest at tiers 2–3 scale, fixed with untagged regression tests
so the normal suite pins them forever.

1. **Default hot-set K ignored the budget** (`stats/rotation.go`).
   The tier defaults (K=500 / K=1,000) predate the budget math: at
   `CommandBudget=2000, Interval=1s` the default hot set alone would spend
   500 × 16 = 8,000 commands per 2,000-command tick — a 4× overrun on every
   tick, before any cold shard. *Fix:* the hot set is bounded at half the
   refresh budget (measured: exactly 41 at these settings); an **explicit**
   `Config.HotSetK` still raises the ceiling (the documented visible-overrun
   escape hatch). Regression: `TestPlanSweepDefaultHotKBudgetClamp`.

2. **A mass incident starved the cold rotation** (`stats/rotation.go`).
   Attention-flagged queues joined the hot set unbounded. A fleet-wide event
   (every queue `NO_CONSUMERS` — exactly what a total worker outage looks
   like) inflated the hot set until the cold shard hit its 1-queue minimum
   quantum: rotation estimates exploded (400 s for a 100-queue fleet under
   the small-budget integration fixture) and budget guarantees evaporated at
   the precise moment Redis protection matters most. *Fix:* the bounded hot
   set fills in priority order — viewed → attention (severity-ranked report
   order) → top-K scored — and everything past the ceiling stays on
   rotation cadence with honest `refreshed_at` stamps.
   Regression: `TestHotSetSelection` ("mass incident" leaves).

3. **Shard sizing ignored the write half of the sweep** (`stats/rotation.go`).
   Shards were sized at 16 commands/queue (the read cost) against 100% of
   the tick budget, but every refreshed row also costs 2 write commands
   (HSET+PEXPIRE), plus per-sweep fixed overhead — a structural ~13%
   overspend before burst work. *Fix:* shards are budgeted at the full
   18-command tick cost against a refresh budget of 75% of the tick, the
   other 25% reserved for auxiliary spend (below).
   Regression: `TestTickCommandBudgets`, updated `TestPlanSweepBudgetMath`.

4. **Unbudgeted auxiliary bursts — including a recurring hourly hump**
   (`stats/series.go`, `stats/baseline.go`). Periodic loads arrived as
   bursts invisible to the planner:
   - every hot-ring series key flushes on the same tick when the sweep clock
     crosses a 30 s slot boundary (~2 commands × every hot key at once);
   - **every UTC hour, every cold queue's rollup accumulator period
     advances, so each queue's next rotation visit flushes its
     previous-hour slots — a sustained ~250 flush-ops/tick wave lasting one
     full rotation, every hour.** The harness caught this red-handed: a run
     whose window straddled 14:00 UTC measured 2,727 cmds/tick max (1.36×
     budget) and a 30,165-key flush wave, while boundary-free runs measured
     1,873. The first drain design (flat cap, but at least ¼ of the backlog
     per tick) simply matched the wave's inflow — backlog-proportional
     drains are the wrong tool for recurring humps;
   - every queue's 7-day fail baseline goes stale on the same tick at UTC
     midnight — at 5,000 queues a single 35,000-command burst (17× budget);
   - the hourly depth-baseline pass re-read every queue's rollup ring in one
     tick (5,000 commands), and a failover holder seeded consumer windows
     for the whole inherited cache at once.
   *Fix:* series flushes queue and drain at a **flat** per-tick cap carved
   from the auxiliary reserve (the backlog absorbs the hourly hump — peak
   ≈ 7 × fleet ops ≈ 1–2 MB — and clears long before the next hour), with a
   memory-relief valve for pathological interval ≥ ~15 s configs; baseline
   loads process a capped, deterministically-ordered slice per sweep and
   resume next tick. Tier 1 is exempt from all caps — the §5.1 table says
   the budget does not govern it, and its behavior (and exact test-pinned
   command counts) is unchanged. With the flat cap the same hour-straddling
   window measures max 2,006 cmds/tick — inside the 2,400 allowance.
   Regressions: `TestSeriesFlushSpread` (slot burst, hourly hump, valve),
   `TestBaselineLoadCaps`.

## Caveats

- **Single local Redis, single replica.** Latency is ~50 µs, not
  production-network; commandstats is server-wide, valid only because the
  harness keeps the measurement window free of test traffic. Failover
  handover cost (cache reload: ~2 + N commands on the first sweep of a new
  holder) is analyzed but not wall-clock-measured here.
- **Attention flood is the fixture's steady state.** With no live workers,
  `NO_CONSUMERS` flags every observed queue. This is the harsh case for the
  hot-set ceiling; a healthy fleet's attention set is far smaller.
- **Delayed rollup slots.** Under the flat-cap drain, a completed hourly
  rollup slot can appear in Redis a couple of minutes after the hour at
  5,000 queues (longer toward the 50,000-queue series ceiling). Readers
  already treat unflushed slots as no-sample; ordering is preserved.
- **GROUP_STALL worst case is not exercised.** Its documented bound
  (25 SMEMBERS + 500 ZRANGE per sweep) exceeds the auxiliary reserve at
  small budgets; the fixture's 5 grouped queues cost a handful of commands.
- **Scheduler-entry snapshots are per-entry per-sweep** (1 HGETALL per known
  entry) and not covered by the budget; fleets with thousands of scheduler
  entries would need the same cap treatment.
- **UTC-midnight fail-baseline reload** now spreads across sweeps by design;
  during the spread, per-queue FAIL_SPIKE detection lags up to
  `fleet ÷ (aux/4 ÷ 7)` ticks (≈5 min at 5,000 queues, 2,000 cmds/s, 1 s).
- The tier-3 name-set uses raw `SADD` for 24,800 empty queues (documented
  exception in the test); all stateful fixtures go through the real client.

## Re-running

```
# both tiers (~7 min total wall clock)
go test -tags scaletest -run TestScale -v ./stats/ -timeout 15m

# individually
go test -tags scaletest -run TestScaleTier2Fleet5000 -v ./stats/ -timeout 15m
go test -tags scaletest -run TestScaleTier3SpotCheck25000 -v ./stats/ -timeout 15m
```

Do not run anything else against Redis DB 14 concurrently (the normal
`go test ./stats/` suite flushes it). The harness flushes DB 14 when done.
