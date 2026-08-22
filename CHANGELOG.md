# Changelog

All notable changes to this project will be documented in this file.

The format is based on ["Keep a Changelog"](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- (api): Fixed a remotely-triggerable panic — a hostile `?page=` value overflowed the search pagination arithmetic past the bounds clamp (review P1-1)
- (api): The legacy task-search scan budget is now total per request instead of per queue; `queue=all` on a large fleet could walk `max_scan × #queues` tasks and buffer every match in memory (review P1-2)
- (api): `POST /api/tasks:batch_filtered` is now audited (actor, verb, scope, reason, counts) and throttled; it was the one bulk mutation path with no audit trail and no rate limit (review P1-3)
- (identity): Loud startup warning when `AuthHeader` is set without `TrustedProxies` — the header was silently trusted from every peer, letting any direct client forge the audit actor (review P2-2)
- (deps): `rs/cors` bumped past GO-2024-2883 (preflight header DoS); `go.mod` raised from the stale `go 1.16` to 1.21
- (docker): The `scratch` image now ships CA certificates (TLS Redis / HTTPS Prometheus failed x509 verification) and runs as uid 65534 instead of root

### Fixed

- (jobs): Paused jobs are parked unclaimed — a re-claim after a replica restart silently resumed enumeration while the UI said "paused", and claimed-paused jobs pinned runner concurrency slots indefinitely; cancel-while-paused finalizes store-side; the preview group cursor is cleared at queue boundaries (a resume at the boundary under-counted the preview the delete gate trusts); control transitions compare-and-set on the state (review P1-4)
- (jobs): Candidate lists are capped at 1M refs with an honest scope-too-large failure — a fleet-wide preview could persist gigabytes in Redis for the 30-day TTL (review P2-9)
- (stats/errsig): Series flushes, scheduler-snapshot upserts, and the errsig feeders are fence-guarded; a stalled ex-holder could rewind series ring headers, stomp slots with stale gap sentinels, or double-count archived signatures after a failover; a superseded sweeper no longer publishes its sweep locally before noticing the rejection (review P2-3)
- (errsig): The archived-tail feeder pages trim-safely — asynq's archive trim shifted the old fixed-Min/offset window so entries were skipped and lost behind the cursor, precisely on at-cap queues (review P2-3)
- (stats): The cache index SET gets the same safety-net TTL as the rows; hot-set top-K slots are no longer burned on already-admitted queues; carried-forward cold rows get live consumer counts so NO_CONSUMERS onset/clear doesn't lag a full rotation (review P3-3)
- (observe): Attempt keys gain an `att` namespace segment — a queue literally named `sum` could collide an attempt LIST with a summary HASH and drop both records (breaking for records written under the old layout; 7-day TTL data) (review P3-3)
- (api): Read-only rejections return the JSON error shape; `Close` runs every closer; `cancel_all` snapshots ids before canceling; task type reaches `ResultFormatter` on the detail endpoint; views/markers/hygiene bodies are size-capped; the legacy queue endpoints bound per-request Redis fan-out; single-queue `state_counts` 404s for missing queues; free-text search matches binary payload bytes (review P2-7)
- (ui): Library embedding under `RootPath` works again — the Vite migration hardcoded `/assets/...` URLs and sub-path deployments rendered a blank page (review P1-7)
- (ui): Out-of-order fetch responses are discarded in the Tasks/Queues consoles — a slow response for an old filter could overwrite a newer one's rows/total/cursor (review P1-9)
- (ui): The Metrics page defaults to a 1-hour window; it fetched a 60-second window with a blank duration selector (the param is seconds), and the backend 400s non-positive durations instead of surfacing Prometheus 502s (review P1-8)
- (ui): Page-level destructive shortcuts (x/#/e/r/j/k) are muted behind every modal surface — they used to fire on the hidden selection under the task drawer and confirm dialogs; icon-only pagination and row-action buttons carry accessible names (review P2-4)
- (ui): OpsView clears the verify verdict and failure report when switching expanded jobs (job B showed job A's verdict); the jobs list keys its row fragments so SSE-prepended jobs stop shifting row identity (review P2-5)
- (ui): The fleet SSE stream gets a 60s no-event watchdog (a half-open connection suppressed the poll fallback indefinitely) and partial fetch failures surface instead of reporting "fresh" (review P2-9)
- (helm): The liveness probe is process-level (TCP) — probing `/healthz` restart-looped asynqmon pods during Redis outages; readiness keeps tracking Redis (review P2-8)
- (release): The workflow runs on maintained actions, ships a real `.exe` in a zip for Windows, adds darwin/arm64 + linux/arm64, and publishes SHA256 checksums (review P2-1)

### Added

- (errsig): `Config.CommandBudget` (default 500 cmds/s) governs both indexer feeders with holder-local rotation — a run that fits the budget behaves as before; larger fleets are covered across ticks; the per-queue metadata pipeline is chunked (review P2-3, closing the last scale item)
- (cmd): The binary refuses `--require-identity` + `--auth-header` without `--trusted-proxies` — a spoofed header must not satisfy the identity requirement (review P2-2; the library keeps the warning)
- (ui): Pause/resume from Queues-directory rows (review P1-10); one shared refcounted jobs EventSource per tab (review P2-9); failure-grouping names unified — the indexed page is "Error signatures", live `error~` aggregations are "Top errors" everywhere (review P3-1)
- (stats): `counterDelta` recovers the UTC-midnight counter tail for cold queues via two budgeted GETs per rolling queue per day (review P3-3)
- (jobs/ui): Preview-only `count` verb — scan-to-completion counting no longer masquerades as `delete`/`archive` in the Operations screen and audit log; count jobs are refused by the execute gate and render a muted chip (review P2-5)
- (ui): Queue pause / resume / delete controls in the queue workspace header, honoring read-only mode — the endpoints existed since upstream with no UI entry point (review P1-10)
- (ui): Saved-view management in Settings (open/rename/delete, shared-asset warning); a `?` keyboard-cheatsheet chip in the topbar; disabled-with-tooltip affordances naming `--enable-enqueue` for Clone & edit and Schedulers Run-now; first-run guidance in the Tasks empty state (review P3-2)
- (ci): CI workflow running go vet + tests (with a Redis service so integration suites actually run), UI lint/tests/build, an embedded-`ui/build` drift check, and helm lint; dependabot now watches gomod, github-actions, and docker (review P1-5)
- (docs): README documents the console — AQL grammar with examples, the Queues filter grammar, Operations/bulk jobs/audit, Hygiene, saved views, the keyboard map, and the identity/audit trust model; five previously undocumented flags added to the flag table (review P2-6)
- (docs): `docs/review/2026-08-20-full-review.md` — the full review backlog with per-item status

- (docker): Multi-arch images — `linux/amd64` + `linux/arm64` manifests published by CI; Dockerfile cross-compiles via BuildKit `TARGETOS`/`TARGETARCH` (upstream hibiken/asynqmon#292)
- (cmd): Added `--redis-username` / `REDIS_USERNAME` — redis ACL username for single, cluster, and sentinel modes (upstream hibiken/asynqmon#273)
- (cmd): Added `--redis-sentinel-password` / `REDIS_SENTINEL_PASSWORD` — authenticates to the sentinel nodes, separate from `--redis-password` for the servers behind them (upstream hibiken/asynqmon#349)
- (pkg/cmd): `GET /healthz` liveness/readiness probe — PINGs redis with a 2s budget, 200/503 JSON; Helm chart probes now point at it (upstream hibiken/asynqmon#276)
- (pkg/cmd): Added `Options.PrometheusBasicAuth` and `--prometheus-basic-auth` / `PROMETHEUS_BASIC_AUTH` (`user:password`) — basic-auth on proxied Prometheus queries, never logged (upstream hibiken/asynqmon#248)
- (pkg/cmd/ui): Full payload on the task DETAIL endpoint — `Options.DetailPayloadFormatter`/`DetailResultFormatter` + `--max-detail-payload-length` / `MAX_DETAIL_PAYLOAD_LENGTH` (default 262144, 0 = unlimited); list cells stay capped by `--max-payload-length`, the drawer shows the whole payload with an honest "truncated at N chars" note at the cap and a copy-payload button (upstream hibiken/asynqmon#301)
- (pkg/ui): `POST /api/schedulers/{stable_key}/run` — run a scheduler entry's task NOW (fresh enqueue of the entry's type/payload, not a scheduler tick) through the flag-gated enqueue client; works on live and GONE entries, reconstructs queue/retry/timeout/retention/unique options, declares `applied_options`/`skipped_options`, gated by `--enable-enqueue` + not read-only, audit verb `run_scheduler_entry`; Schedulers screen grows a per-row Run-now with a two-step confirm (upstream hibiken/asynqmon#337)

## [0.7.0] - 2022-04-11

Version 0.7 added support for [Task Aggregation](https://github.com/hibiken/asynq/wiki/Task-aggregation) feature

### Added
 
- (ui): Added tasks view to show aggregated tasks

## [0.6.1] - 2022-03-17

### Fixed
- (ui): Show metrics link in sidebar when --prometheus-addr flag is provided

## [0.6.0] - 2022-03-02

### Added

- (cmd): Added `--read-only` flag to specify read-only mode
- (pkg): Added `Options.ReadOnly` to restrict user to view-only mode
- (ui): Hide action buttons in read-only mode
- (ui): Display queue latency in dashboard page and queue detail page.
- (ui): Added copy-to-clipboard button for task ID in tasks list-view page.
- (ui): Use logo image in the appbar (thank you @koddr!)

### Fixed
- (ui): Pagination in ActiveTasks table is fixed

## [0.5.0] - 2021-12-19

Version 0.5 added support for [Prometheus](https://prometheus.io/) integration.

- (cmd): Added `--enable-metrics-exporter` option to export queue metrics.
- (cmd): Added `--prometheus-addr` to enable metrics view in Web UI.
- (pkg): Added `Options.PrometheusAddress` to enable metrics view in Web UI.

## [0.4.0] - 2021-11-06

- Added "completed" state
- Updated to be compatible with asynq v0.19

## [0.3.2] - 2021-10-22

- (ui): Fixed build

## [0.3.1] - 2021-10-21

### Added

- (cmd): Added --max-payload-length to allow specifying number of characters displayed for payload, defaults to 200 chars
- (pkg): DefaultPayloadFormatter is now exported from the package

## [0.3.0]

### Changed

- Asynqmon is now a go package that can be imported to other projects!

## [0.2.1]

### Addded

- Task details view is added
- Search by task ID feature is added

## [0.2]

### Changed

- Updated to depend on asynq 0.18

## [0.1.0-beta1] - 2021-01-31

Initial Beta Release 🎉
