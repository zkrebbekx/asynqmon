# Full code / product / design review — 2026-08-20

A structured review of the fork across five dimensions: backend core (HTTP
layer), backend subsystems (stats/hygiene/errsig/jobs/observe/leasefence),
frontend (ui/src), product/UX, and build/CI/packaging.

> **Note:** GitHub Issues are disabled on this fork, so the backlog lives here.
> Enable them under *Settings → General → Features → Issues* to move these to
> the tracker. Each entry below is written to be lifted verbatim into an issue.
>
> Status legend: `[ ]` open · `[x]` fixed in this review branch · `[~]` partially fixed.
>
> **Status as of the end of the review session** — what remains open:
>
> - **P1-10 [~]**: pause/resume/delete shipped in the queue workspace header;
>   Queues-directory row actions are still a nice-to-have.
> - **P2-2 [~]**: loud startup warning shipped + README trust-model docs;
>   consider hard-refusing `RequireIdentity` without `TrustedProxies` in a
>   breaking release.
> - **P2-3 [~]**: series/scheduler/errsig writes are fenced and tail paging is
>   trim-safe; the errsig indexer still has **no command budget/tiering** at
>   tier-2/3 fleet sizes (largest remaining scale-engineering item).
> - **P2-8 [~]**: liveness probe + rs/cors + go.mod + dependabot shipped;
>   cutting an actual tagged release (chart appVersion, CHANGELOG release)
>   is a process step for the maintainer.
> - **P2-9 [~]**: fleet-stream watchdog + candidates cap shipped; useJobsEvents
>   still opens one EventSource per consumer (HTTP/1.1 6-per-host pressure).
> - **P3-1 [~]**: fleet/§-jargon strings fixed; unifying the four names for
>   failure grouping (Errors / signatures / clusters / top errors) is a
>   product-naming decision left open.
> - **P3-3 [~]**: index TTL, hot-set slots, live consumer counts, observe key
>   collision shipped; `counterDelta`'s UTC-midnight tail loss remains (needs
>   an extra per-queue read at rollover, to be budgeted).

Baseline at review time: `go build`, `go vet`, full Go test suite (9 packages)
and all 505 UI tests pass; embedded `ui/build` is in sync with `ui/src` at HEAD.

---

## P1 — fix first

### [x] P1-1 · Remotely-triggerable panic: unclamped `page` in /api/tasks; negative `failures_offset` reaches LRANGE

**Area:** backend · **Type:** bug/security

Two missing input clamps let arbitrary query params produce a server panic or
a nonsense response window.

1. **Panic via `page` overflow** — `task_search_handlers.go:377-386` (same
   pattern in the AQL scan path ~:505):

   ```go
   start := (page - 1) * size
   if start > total { start = total }
   end := start + size
   if end > total { end = total }
   pageTasks := matches[start:end]
   ```

   `page` comes from `atoiDefault(q.Get("page"), 1)` with only a `< 1` floor.
   `GET /api/tasks?page=4611686018427387904&size=20` wraps `(page-1)*size`
   negative; the `start > total` clamp doesn't catch a negative value, and
   `matches[start:end]` panics. Any anonymous client gets a guaranteed
   500/connection-abort plus a stack trace in the logs, per request.

2. **Negative `failures_offset`** — `jobs_handlers.go:182` +
   `jobs/store.go:306`: no clamp before `LRANGE`, and Redis resolves a
   negative start as `len+offset`, returning a window pulled from the list
   tail instead of a 400. The sibling results endpoint clamps
   (`jobs_handlers.go:245`); this one was missed.

**Fix:** clamp `start < 0 → 0` (or cap `page`) in both paging paths; clamp
`failures_offset < 0 → 0`; add hostile-input regression tests.

### [x] P1-2 · Legacy task search: scan budget multiplies per queue and the match set is unbounded — one request can OOM the server

**Area:** backend · **Type:** bug/security

`task_search_handlers.go:38,284-316`: `defaultMaxScan` (10k) is applied **per
queue per request** (`qScanned` resets for every queue), and `resolveQueues`
expands the default `queue=all` to every queue. On a 500-queue fleet,
`GET /api/tasks?q=x&max_scan=100000` walks up to 50M tasks in one request.
Worse, `matches` (each holding the full `rawPayload` string) grows unbounded
across queues — only the scan is capped, never the match set (`q=""` matches
everything). The AQL scan path got this right (`aql_exec.go:365` — "Budget is
TOTAL per call"); the legacy path and `collectAqlMatches` callers were left on
the multiplied budget.

**Failure scenario:** one dashboard user with the archived tab open on a large
fleet OOMs the replica.

**Fix:** make the scan budget total-per-request on the legacy path too, and cap
the collected match set (report `truncated` — the response shape already
carries it).

### [x] P1-3 · `POST /api/tasks:batch_filtered` bypasses the audit log and throttling for mass mutations

**Area:** backend · **Type:** bug/security

`task_search_handlers.go:1002-1020`: every other fork mutation path writes to
the audit stream (`AppendAudit` in jobs/enqueue/markers/views/hygiene/
scheduler-run handlers), and the jobs runner exists precisely to throttle bulk
verbs at 100–1000/sec with preview + reason + typed confirmation. This
endpoint can delete tens of thousands of tasks matching a filter with **zero
audit trace, no actor attribution, and one sequential un-throttled Redis round
trip per task**. The most destructive bulk path is the one that bypasses every
safeguard.

**Fix:** append an audit entry (actor, verb, filter, affected count) and apply
a modest rate limit / hard cap; long term, steer the UI fully onto the jobs
API.

### [x] P1-4 · Jobs runner: paused jobs resume scanning when re-claimed; stale `cursor_groups` applied across queue boundaries; paused jobs pin concurrency slots

**Area:** backend (jobs) · **Type:** bug

1. **Pause not honored across re-claim** — `jobs/runner.go:335`:
   `claimableState` includes `StatePaused`, and the preview branch
   (`if !j.PreviewComplete { r.enumerate(...) }`) runs with **no state
   check**; `checkCtl` reacts only to the `ctl` field, already cleared when
   the pause was honored. Scenario: operator pauses a preview to relieve
   Redis; the claiming replica restarts; another replica claims within one
   poll and enumeration continues at full speed while the UI shows "paused".
   The execute-phase path checks (`runner.go:352`); only the preview branch
   is missing it.

2. **Stale group cursor across queues** — `jobs/runner.go:509-512,596-609`:
   finishing a queue's last group persists `cursor_qidx=qi+1, cursor_gidx=0`
   but never clears `cursor_groups`. On re-claim, `if qi == startQ &&
   j.cursorGroups != nil { groups = j.cursorGroups }` enumerates queue B
   against queue A's frozen group list — B's real groups are never listed, so
   the "completed" preview silently under-counts (and the delete gate trusts
   that count).

3. **Paused jobs starve the fleet** — `jobs/runner.go:389-415` +
   `claimTick:234-243`: `waitWhilePaused` polls forever while holding a
   concurrency slot, and oldest-first claiming grabs old paused jobs first.
   With default `Concurrency = 2`, two forgotten paused jobs pin a replica;
   paused jobs ≥ 2× replicas halts all job processing fleet-wide.

4. **Unfenced cancel/execute race** — `jobs/store.go:364-432`: a cancel of a
   `preview_ready` job can be overwritten by a racing execute (stale
   check-then-write), resurrecting a canceled job into `running`.

**Fix:** guard the preview branch on `StatePaused` (park, don't enumerate);
clear/re-key `cursor_groups` on queue advance; release the slot while paused
(or skip paused jobs in `claimTick` and re-claim on resume); make control
transitions compare-and-set.

### [x] P1-5 · No CI runs tests or lint; embedded ui/build drift is unchecked

**Area:** build/CI · **Type:** gap

`.github/workflows/` contains only CodeQL, docker publish, and release. The
repo has ~30 Go test files, a 505-test vitest suite, and an `oxlint` script —
none executed anywhere. No `go vet`, no helm lint, and no check that the
committed `ui/build` matches `ui/src`. This already bit: commit `2f36d6b`
changed `TasksGlobalView.tsx` without regenerating `ui/build`; drift was only
fixed because the next commit happened to rebuild.

**Fix:** add a CI workflow running `go vet` + `go test ./...`, `npm ci &&
npm run lint && npm test`, and an embedded-build drift check (rebuild and
diff, or hash manifest). Lint config should also exclude `ui/build` (oxlint
currently warns on the bundled dayjs).

### [x] P1-6 · `scratch` Docker image has no CA certificates — documented TLS features fail; runs as root; EOL unpinned base images

**Area:** build/packaging · **Type:** bug

- `Dockerfile:59-65` copies only the binary into `FROM scratch`. The binary
  supports `--redis-tls`, `rediss://` URLs, and HTTPS `--prometheus-addr` —
  all need root CAs; the pool in scratch is empty. Anyone pointing the
  published image at managed TLS Redis (ElastiCache, Upstash…) gets
  `x509: certificate signed by unknown authority` and is pushed toward
  `--redis-insecure-tls`. One-line fix: `apk add ca-certificates` in the
  build stage and `COPY` the bundle.
- No `USER` in the final stage → uid 0 under plain `docker run` (Helm chart
  mitigates; raw Docker doesn't).
- `golang:1.21-alpine` is EOL (no security patches); base images are tag-only,
  not digest-pinned.
- `Dockerfile:36-39`: `COPY . .` brings the committed `ui/build` into the
  backend stage, then the fresh UI build is COPYed **into** (merged with) the
  same dir — stale content-hashed chunks survive and get embedded via
  `//go:embed`. `.dockerignore` should exclude `ui/build`.

### [x] P1-7 · Vite migration broke `RootPath` sub-path embedding for library users

**Area:** build/backend · **Type:** bug

`ui/build/index.html` hardcodes absolute asset paths
(`<script src="/assets/index-*.js">`); only the favicon and window globals go
through the `/[[.RootPath]]` template (`static.go:76-86`). `ui/vite.config.ts`
sets no `base`, and the CRA `homepage` mechanism the static.go comment still
references no longer exists. README documents `RootPath: "/monitoring"`; with
that config every asset request goes to `/assets/...`, outside the
`PathPrefix(opts.RootPath)` router → blank page. `static_test.go` only tests
`rootPath: ""`.

**Fix:** template the asset URLs through RootPath (e.g. Vite
`base: "/[[.RootPath]]/"` with the same collapse trick as `window.ROOT_PATH`,
or `experimental.renderBuiltUrl`), and add a `rootPath: "/monitoring"` case to
`static_test.go` asserting asset URLs are prefixed.

### [x] P1-8 · Metrics view defaults to a 60-second window and a blank duration selector

**Area:** frontend · **Type:** bug

`ui/src/views/MetricsView.tsx:32`:
`const duration = parseInt(query.get("duration") || "60", 10);` — the
backend's `duration` param is **seconds** (`metrics_handler.go:168`), and the
UI always sends it, so the server's own 60-minute default never applies. First
visit to `/q/metrics` fetches a 1-minute window, and `MetricsFetchControls`'
`<Select value={String(duration)}>` matches none of its options (smallest is
`30*60`) so the trigger renders empty. The chart looks broken by default.

**Fix:** default to `3600` (any value present in `durationOptions`). Backend
side: `/api/metrics` should also 400 on non-positive `duration`/bad `endtime`
instead of proxying an invalid range to Prometheus (returns a 502 today).

### [x] P1-9 · Tasks console: a stale in-flight response can overwrite a newer filter's results

**Area:** frontend · **Type:** bug

`ui/src/views/TasksGlobalView.tsx:344-389`: `fetchTasks` has no request
sequencing, abort, or "is this response still current?" guard —
`setTasks(rows); setTotal(resp.total); setMeta({...cursor...})` runs
unconditionally on resolve. Clicking a state pill while a slow request for the
previous filter is in flight can leave the table showing the previous query's
rows, total, and cursor under the new pill until the next poll tick (default
8s). The whole-scope bulk bar then advertises "all N matching" using the stale
total. Same unguarded pattern in `fetchCounts`/`fetchFacets`/`fetchAnalytics`
and `QueuesDirectoryView.fetchQueues` (lower impact).

**Fix:** sequence number or AbortController per filter key; discard
non-current responses.

### [~] P1-10 · Queue pause / resume / delete have no UI entry point

**Area:** product/frontend · **Type:** gap

`handler.go:442-444` registers `DELETE /api/queues/{qname}`, `:pause`,
`:resume`; `ui/src/actions/queuesActions.ts:121-169` defines the async
actions; `DeleteQueueConfirmationDialog.tsx` exists — but there are **zero
call sites** in any view. The Queues directory and workspace show "paused"
chips with no toggle. Worse, `BulkJobModal.tsx:335` actively recommends
"**Pause the queue first** — the list stops churning, then drain
(recommended)" — advice the product cannot follow. Pausing a misbehaving
queue is the on-call operator's first remediation lever; today they need
`asynq` CLI access mid-incident.

**Fix:** surface pause/resume (and delete, behind the existing confirmation
dialog) in the queue workspace header and/or Queues directory row actions,
honoring read-only mode.

---

## P2 — important

### [x] P2-1 · Release workflow uses deprecated/dead actions; Windows artifact is broken; no darwin/arm64; no checksums

**Area:** build/CI · **Type:** bug

`.github/workflows/release.yml`: `actions/checkout@v2` (node12-era),
`bruceadams/get-release@v1.2.2` (third-party, tag-pinned),
`actions/upload-release-asset@v1` (archived since 2021) — the next release run
is likely to fail on retired runtimes. `GOOS=windows … make build` then
`tar -czvf …windows_amd64.tar.gz asynqmon` ships a PE binary named `asynqmon`
with no `.exe` inside a tarball. No Apple Silicon (darwin/arm64) build, no
checksums file. Also: zero git tags exist, so this workflow and the semver
docker tag rules have never fired (see P2-8).

**Fix:** modernize (checkout@v4 + `softprops/action-gh-release` or goreleaser),
add `.exe` + zip for Windows, darwin/arm64, and a SHA256SUMS file.

### [~] P2-2 · Identity: `AuthHeader` is trusted from any peer when `TrustedProxies` is empty — `RequireIdentity` satisfiable by a spoofed header

**Area:** backend · **Type:** security

`identity.go:104-106`: `if len(trusted) == 0 { return true }`. When an
operator sets `AuthHeader` (e.g. `X-Auth-Request-User`) but forgets
`TrustedProxies`, any client that can reach the listener directly (sidecar
bypass, cluster-internal access) forges an arbitrary actor: audit entries are
attributed to a victim and `RequireIdentity` is fully satisfied by a spoofed
header. Insecure-by-default in exactly the deployment that signals the
operator cares about attribution.

**Fix:** at minimum log a prominent startup warning when
`AuthHeader != "" && len(TrustedProxies) == 0`; consider refusing
`RequireIdentity: true` without `TrustedProxies`. Document the trust model in
the README flag table (see P2-6).

### [~] P2-3 · Fencing gaps outside the stats cache; errsig indexer has no command budget and a paging skip bug

**Area:** backend (stats/errsig) · **Type:** bug

1. **Series/scheduler writes bypass the fence** — `stats/series.go:715-745`,
   `stats/scheduler.go:182-234`: only `writeCache` runs through `fence.Exec`;
   series flushes use a plain pipeline guarded by the stale holder's
   **in-process** header cache, so a woken ex-holder can `SETRANGE` the header
   `last` backwards and hide the new holder's freshest slots from every chart
   until the next flush (self-healing but a real §5.13 violation).
2. **Off-lease `SweepTailNow` double-counts** — `errsig/indexer.go:352-354,
   494-522`: the archived-tail merge is a non-idempotent read-modify-write
   (`c.Archived += n`) written with token 0, which `leasefence.Exec` executes
   **unfenced**. A concurrent off-lease run (standby operator trigger) merges
   the same entries twice; the "EXACT" archived counts inflate monotonically,
   never corrected. Cursors also race.
3. **Superseded sweeper publishes before fence check** — `stats/engine.go:
   824-849`: `e.mem.replace(...)` + `notifySweeps()` run before `!fenceOK`
   stands the replica down, so an ex-holder serves diverging numbers as
   `SourceLocal` for up to `localStaleAfter` after a failover.
4. **No errsig budget/tiering** — `errsig/indexer.go:371-380,651-658`: every
   15s one unchunked N×3-command pipeline (75k commands at 25k queues) plus
   up to 200 sequential `GetTaskInfo` per queue per minute — the 2,000 cmds/s
   governor SCALE.md documents covers only `stats.Engine`; at tier-2/3 the
   indexer alone can dwarf it.
5. **Offset paging skips entries under concurrent trim** —
   `errsig/indexer.go:423-481`: fixed `Min` score + growing `Offset` while
   asynq's archive trim removes lowest-scored members shifts pages left, so
   entries are skipped and lost behind the advanced cursor — precisely on
   at-cap queues. Advance `Min` per page instead of using offsets.

### [x] P2-4 · Modal overlays don't mute the console keymap — destructive shortcuts fire behind the drawer

**Area:** frontend/a11y · **Type:** bug

`App.tsx:116-120` sets the `OVERLAY_ATTR` mute only for the palette and
cheatsheet. TaskDrawer declares `aria-modal="true"` and traps Tab, but the
document-level keymap (`j/k/x/shift+x/p/r/e/#`) stays live while it (or
`ConfirmDialog`) is open: with rows selected, `#` inside the drawer opens the
delete-selected confirm behind/over the drawer; `x` mutates the hidden
selection; `j/k` move the invisible focus ring. A keyboard hazard and an a11y
defect. Also: icon-only pagination chevrons and row-action buttons
(`TasksGlobalView.tsx:1398-1415`, `TasksTable.tsx:413-430`) have no accessible
names — screen readers announce "button".

**Fix:** stamp `OVERLAY_ATTR` (or disable the keymap) while peek/confirm
dialogs are open; add `aria-label`s.

### [x] P2-5 · Ops view correctness: stale verify verdict on newly expanded job; keyless list fragments; "count" scan jobs recorded as DELETE in the audit log

**Area:** frontend/product · **Type:** bug

1. `OpsView.tsx:359-405`: `verify` is only reset in the `!expanded` branch —
   clicking row B while A is expanded shows **A's verify verdict attributed
   to B** until Re-check (the auto-verify effect bails because
   `verify === null` is false). `detail` similarly flashes A's failure report.
2. `OpsView.tsx:474-475,708`: `rows.map` returns keyless `<>` fragments (keys
   on inner rows don't count); fresh SSE jobs are *prepended*, so every
   existing row shifts identity — React warns and reconciles positionally.
3. **Audit honesty** — `TasksGlobalView.tsx:86-97`: scan-to-completion count
   jobs are created with the "least destructive verb the state supports" —
   `delete` for completed — so OpsView renders a red DELETE chip and the audit
   log records "delete job created" for what is a read-only count, mitigated
   only by the reason string. Admins reviewing the trail see phantom deletes;
   operators learn to ignore delete entries. A dedicated `count`/`preview`
   verb would be honest.

### [x] P2-6 · README documents almost none of the shipped product; five operational flags missing from the flag table

**Area:** docs/product · **Type:** gap

README covers flags, Prometheus, the Tasks view, Flow view, and observe
middleware — nothing about: Overview/attention engine, Queues directory filter
grammar (`consumers=0 pending>10000 name~email`), **AQL search syntax** (zero
written documentation anywhere for the flagship console's query language),
Errors/signature explorer, Operations (bulk jobs, audit log), Hygiene reports,
Schedulers outcome traces, saved views, the command palette, SSE live
streaming, deploy markers. And `cmd/asynqmon/main.go:122-126` defines
`--stats-interval`, `--disable-stats`, `--auth-header`, `--trusted-proxies`,
`--require-identity` — none in the README flag table, though the audit trail
is unattributed without `--auth-header` (and spoofable without
`--trusted-proxies`, see P2-2).

### [x] P2-7 · API robustness batch: unbounded bodies, bare-text read-only 405s, Close aborts early, empty type to ResultFormatter, cancel_all pagination, legacy queue-endpoint Redis storms

**Area:** backend · **Type:** bug

1. **Unbounded request bodies** — `views_handlers.go:132,182`,
   `markers.go:150`, `hygiene_handlers.go:171`: raw `json.NewDecoder(r.Body)`
   with no `http.MaxBytesReader` (9 other call sites wrap it); size caps are
   checked only after the decoder buffered the full token — a multi-GB body
   is held in memory before rejection.
2. **Read-only 405s are bare text** — `handler.go:769-777`: every read-only
   rejection except enqueue returns `text/plain` via `http.Error`, violating
   the `{"error": ...}` contract every other path honors; JSON-parsing
   frontends surface a cryptic parse failure instead of the reason.
3. **`HTTPHandler.Close` aborts on first error** — `handler.go:364-371`:
   an early closer error leaks the redis client, inspector, and engine
   goroutines. Run all closers, join errors.
4. **Empty task type to ResultFormatter** — `conversion_helpers.go:232`:
   `toTaskInfo` passes `rf.FormatResult("", info.Result)` while
   `toCompletedTask` correctly passes `ti.Type` — embedders dispatching on
   type silently misformat the task drawer.
5. **`cancel_all` pagination misses tasks** — `task_handlers.go:89-113`:
   paginates forward across the active list while cancels shrink it; later
   tasks shift into visited pages and never get the signal. Snapshot IDs
   first or re-list page 1 until stable.
6. **Per-request Redis storms** — `queue_handlers.go:38-56,150-160`: one
   goroutine per queue running `GetQueueInfo` unbounded (2,000 simultaneous
   bursts per homepage poll at 2,000 queues); `newListQueueStatsHandlerFunc`
   runs `History(qname, 90)` serially per queue, uncached — O(queues×90)
   reads per request. The stats cache exists precisely to avoid this.
7. Minor: single-queue `state_counts` returns 200 with zeros for a missing
   queue (`task_search_handlers.go:562-573`); view-store cap check fails open
   on Redis error (`views_handlers.go:150`); legacy free-text matches
   formatter *output* so `q=non-printable` matches every binary payload
   (`task_search_handlers.go:134`).

### [~] P2-8 · Deployment/versioning: Helm liveness probe restarts pods on Redis outage; `appVersion: latest`; stale deps (rs/cors CVE); go.mod says go 1.16; Dependabot misses gomod/actions

**Area:** build/deploy · **Type:** bug

1. **Liveness probe** — `charts/asynqmon/values.yaml:100-105`: liveness hits
   `/healthz`, which 503s when Redis is down → kubelet restart-loops asynqmon
   during any Redis outage, adding CrashLoopBackOff to recovery. Readiness on
   `/healthz` is right; liveness should be process-level.
2. **Versioning** — `Chart.yaml` pins `appVersion: latest`, `values.yaml`
   defaults `tag: ""` + `IfNotPresent` → mixed versions across nodes, no
   rollback target. No git tags exist; the release + semver docker workflows
   have never fired; CHANGELOG has a growing "Unreleased" with last release
   0.7.0 (2022 upstream).
3. **Dependencies** — `rs/cors v1.7.0` (2019): versions <1.11.0 affected by
   GO-2024-2883 (preflight DoS), on the serving path when
   `--cors-allowed-origins` is set. `go-redis v9.0.4` (2023),
   `prometheus/client_golang v1.11.1` (2022) majorly stale. `go.mod` declares
   `go 1.16` while README/Dockerfile/CI require 1.21+ (latent, but caps
   language semantics and misadvertises support). `.github/dependabot.yml`
   covers only npm — add `gomod` + `github-actions` ecosystems.

### [~] P2-9 · Frontend resilience batch: sticky SSE health flag defeats poll fallback; per-consumer EventSources; partial fleet fetch clears errors; jobs candidates list unbounded

**Area:** frontend/backend · **Type:** bug

1. `useFleetEvents.ts:141`: `sseHealthy` set true on any event, cleared only
   in `es.onerror`. A half-open connection (server killed without FIN, proxy
   misbehavior — the server's comment heartbeats are invisible to the
   EventSource API) fires no error for minutes, and
   `if (document.hidden || sseHealthy) return;` blocks the poll fallback —
   the Fleet page freezes stale despite the file's own contract. Add a
   watchdog: no event for N×heartbeat → treat unhealthy, reopen + poll.
2. `useJobsEvents.ts:34-88`: one EventSource per consumer; Ops page + modal +
   fleet stream ≥3 SSE connections/tab — HTTP/1.1's 6-per-host cap starts
   starving normal API traffic. Share a singleton per tab.
3. `useFleetEvents.ts:79-92`: `ok` true if *either* overview or attention
   resolves — a permanently failing overview (attention healthy) reports
   "fresh, no error" while the KPI strip silently freezes.
4. **Backend:** `jobs/store.go` candidates list (`RPUSH`, keys :64-66) has no
   cap and no disclosure (failures capped 10k, samples 10): a fleet-wide
   preview can persist tens of millions of `queue\x1fid` refs — GBs in one
   Redis LIST for `jobTTL` (30 days), even for abandoned previews, on the
   production Redis.

---

## P3 — polish / follow-ups

### [~] P3-1 · Terminology and copy cleanup: "fleet" leftovers, internal spec jargon, four names for failure grouping

**Area:** product · **Type:** polish

- Post-rename leftovers users still see: "The **fleet** stats endpoints are
  not answering" (`FleetView.tsx:487`, `QueuesDirectoryView.tsx:457`),
  "**fleet** failures/hr from daily counters" (`FleetView.tsx:370`); Settings
  calls the same subsystem "Console Health" (`SettingsView.tsx:88`).
- Spec jargon in user-facing copy: Operations subtitle "…reviewable,
  query-scoped background job — **§4.3**" (`OpsView.tsx:421`); Errors panel
  "count-over-time arrives with ring buffers (**phase 10**)"
  (`ErrorsView.tsx:180`) — phase 10 shipped, per-signature count-over-time
  didn't; stale roadmap promise.
- One concept, four names: nav "Errors", header "Failure signatures",
  workspace rail "Error clusters", analytics "Top errors", Hygiene "Top error
  clusters" — with subtly different semantics (`error~"prefix"` vs indexed
  signature), so counts can't be sanity-checked against each other.

### [x] P3-2 · Discoverability: keyboard model invisible; flag-gated features vanish without a trace; saved views unmanageable; empty first-run gives no onboarding path

**Area:** product · **Type:** gap

- Rich keymap (j/k/x/⇧x/r/e/#/p/[/]//) exists; the cheatsheet only opens on
  "?" and nothing on screen or in docs reveals that. 8 of its 10 rows only
  work on the Tasks console, unsaid.
- With `--enable-enqueue` unset, "Clone & edit" and the Schedulers Run-now
  column disappear entirely — no disabled state, no tooltip naming the flag,
  though the backend answers an explanatory 403 for exactly this case
  (`handler.go:727-736`). Same for observe: the only pointer to "you could
  have attempt history" is the README.
- Saved views: backend has `PUT`/`DELETE /api/views/{view_id}`
  (`handler.go:653-654`), client bindings exist (`api-views.ts:46-56`), but
  no UI calls them — a mistyped **team-visible** view is permanent clutter;
  the only launcher is the palette's 10-row fuzzy list.
- First run with empty Redis: nine zeroed KPI tiles, "No tasks", and no
  screen ever says "connect an asynq client / enqueue your first task".
- Read-only edges: Hygiene "Run now" stays functional in read-only with no
  explanation (deliberate, `handler.go:695-696`); `/api/queue_stats` has no
  UI caller left (dead surface).

### [~] P3-3 · Subsystem data-honesty batch (stats/observe)

**Area:** backend · **Type:** polish

- `stats/series.go:356-365`: `counterDelta` across UTC midnight discards
  yesterday's uncounted tail — cold queues lose up to one rotation-interval
  of processed/failed deltas per day; fleet sums under-report around 00:00.
- `stats/rotation.go:186-194`: `hotSet` burns top-K slots on queues already
  admitted (increments `taken` on duplicates) — scored component shrinks
  below K during incidents, the moment coverage matters most.
- `stats/engine.go:692-702`: cold rows carry stale `Consumers` into attention
  evaluation though fresh `Servers()` data covers every queue each sweep —
  sev-5 `NO_CONSUMERS` onset/clear lags a full rotation for cold queues.
- `stats/cache.go:138`: `asynqmon:cache:queues` index SET never gets a TTL —
  the one cache key that outlives a dead sweeper forever (up to 50k names).
- `observe/observe.go:106-114`: queue named `sum` collides attempt-LIST keys
  with the summary-HASH namespace (`asynqmon:obs:sum:<id>`) → WRONGTYPE,
  records for both tasks silently dropped.

### [x] P3-4 · Frontend polish batch

**Area:** frontend · **Type:** polish

- `HeaderBar.tsx:40-53`: breadcrumbs omit `p.ERRORS`/`p.HYGIENE` → both pages
  are labeled "Overview" in the chrome, though DESIGN.md assigns page
  identity to the breadcrumb for exactly these dense views.
- `ErrorsView.tsx:110-118`: `.num-cell` is styled only via an arbitrary
  variant on **QueuesDirectoryView's** tbody — ErrorsView's numeric cells
  render unpadded and left-aligned under right-aligned headers.
- `BulkJobModal.tsx:209`: "Open Ops" toast action uses
  `window.location.href` — full SPA reload, drops all in-memory state.
  Use router navigation.
- `paths.ts:21-27`: queue names not URL-encoded in path helpers; queues with
  slashes 404 from directory row clicks (`lib/urlstate.ts:268` explicitly
  anticipates them).
- `localStorage.ts:27` + `store.ts:67-73`: settings persistence rewrites
  localStorage every poll tick just to persist `lastUpdatedAt`, which is
  meaningless across sessions (HeaderBar briefly shows the previous session's
  "updated 3h ago"). Exclude it from persistence.
- `TaskDrawer.tsx:346-356`: copy-feedback `setTimeout`s lack cleanup; can
  flash the previous task's "copied" state after switching tasks.

### [x] P3-5 · Metrics/validation odds and ends

**Area:** backend · **Type:** polish

- `/api/metrics` accepts negative/zero `duration` and arbitrary `endtime`,
  producing start>end Prometheus queries surfaced as 502s instead of a 400
  (`metrics_handler.go:163-169`). (Fixed together with P1-8's frontend half.)

---

## Strengths worth keeping (from the same review)

- **Honesty as a design system**: "no number is better than a wrong one" is
  enforced everywhere — honesty ribbons, "≥" lower bounds, "—" for unknowable
  values, Flow view's "reconstruction, not a trace", the Workers screen
  refusing a heartbeat column asynq can't truthfully fill.
- **Best-in-class destructive-action flow**: streaming exact preview → Redis
  cost-class disclosure → throttle → mandatory audited reason → typed
  confirmation → delete-requires-complete-preview gate → post-run verify
  with live burn-down.
- **`internal/leasefence` and the jobs claim/progress Lua scripts** are
  genuinely correct fencing (token order = claim order; per-chunk fence
  re-check; atomic cursor+append making preview resume exactly-once).
- **The SSE broker** (fleet_events_handlers.go) is a model of leak-free
  streaming: bounded fan-out, wg-tracked shutdown, skip-render-when-idle.
- **URL-as-state + graceful degradation** throughout the frontend; the
  freeze-on-hover "N updates paused" system is cleanly built and tested.
- **Injection defenses**: PromQL quoting, AQL never interpolating user text
  into Redis commands, decompression-bomb bounds, depth-checked msgpack.
