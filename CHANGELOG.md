# Changelog

All notable changes to this project will be documented in this file.

The format is based on ["Keep a Changelog"](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Breaking and API changes since upstream 0.7.2

This fork's HTTP API is a superset of upstream `hibiken/asynqmon` 0.7.2, but
a client written against 0.7.2 sees the differences below. Operational
guidance for the move is in [docs/UPGRADING.md](docs/UPGRADING.md).

### Error bodies and status codes

1. Error bodies are `application/json` `{"error":"..."}`, not `text/plain`
   `"asynq: ..."`. The `asynq: ` prefix is stripped from a 4xx message.
2. `ErrQueueNotFound` and `ErrTaskNotFound` answer `404`, and
   `ErrQueueNotEmpty` answers `400`, on every route. Upstream answered `500`
   on most of them.
3. A Redis outage answers `503`, not `500`. A `context.DeadlineExceeded`,
   any `net.Error`, and the go-redis client and pool errors all map to `503`.
4. A `5xx` body is generic: `{"error":"redis unavailable"}` for `503` and
   `{"error":"internal error"}` for every other `5xx`. The full error, with
   the Redis or Prometheus address it names, goes to the process log.
   Upstream echoed the raw error to the browser.
5. A successful body carries `Content-Type: application/json; charset=utf-8`.

### Request rules that upstream did not have

6. `POST /api/queues/{qname}/tasks` refuses a queue name that
   `Inspector.Queues()` does not list, with `400`. Send `"create_queue":
   true` in the body to create the queue deliberately. It also refuses a
   `deadline` that is not in the future. It accepts an optional `headers`
   map (asynq 0.26 task headers; at most 64 entries, a name of at most 256
   bytes, a value of at most 4096 bytes).
7. `PUT /api/views/{view_id}` requires the `version` field the client read
   from `GET /api/views`. A stale version answers `409 {"error":"view
   changed"}`, and so does a name another view already uses (compared
   without case, on `POST` as well).
8. `POST /api/hygiene/{kind}/run` is throttled to one run per kind per 30
   seconds and answers `429` in between. It also obeys `--read-only` and
   answers `405` there, unless `--hygiene-run-in-read-only` is set.
9. The legacy list endpoints still clamp `size` (`size <= 0` to 20, above
   1000 to 1000), but the payload now discloses it. See item 12.
10. A task scan (`GET /api/tasks`, `/api/task_metadata`,
    `/api/task_aggregate`, `POST /api/tasks:batch_filtered`) clamps
    `max_scan` at 20000, not 100000, and answers `429 {"error":"too many
    concurrent scans"}` with `Retry-After: 1` when more than
    `--max-concurrent-scans` (default 4) scans already run.
    `POST /api/tasks:batch_filtered` refuses more than 2000 matches with a
    `400` that points at `POST /api/jobs`.
11. `GET /api/fleet/events` refuses a subscriber past
    `--max-sse-connections` (default 256) with `503` and `Retry-After: 5`.

### New and changed response fields

12. A legacy list payload carries `page_size_applied` when the handler
    clamped the requested `size`. The field is absent when the request got
    the size it asked for.
13. A task-scan response carries `pending_since_unknown`: the number of
    pending tasks with no `pending_since` record, which `pending_age>` does
    not evaluate. List them with `pending_age=unknown`.
14. `GET /api/schedulers` carries `live_count` on every row: the number of
    live entries registered under that stable key, or 0 for a snapshot-only
    row.
15. A saved view carries `version` and `updated_at`.
16. `GET /api/features` carries `version`, the build version of the serving
    binary. It is `""` when the embedder set none.
17. `GET /api/health/roles` carries an `errsig` object with the indexer's
    last-run command spend. The object is absent on a replica that runs no
    indexer.
18. The `coverage.source` field of `GET /api/fleet/overview` and
    `GET /api/fleet/attention` may answer `"stale-cache"`: the replica's
    last successful cache read, served with its original `coverage.updated_at`
    while the shared cache is gone. Upstream had no such endpoints; a client
    that reads `coverage.source` must accept the third value next to `local`
    and `cache`.
19. `pending_tasks[].pending_since` is added.
20. `servers[].active_workers[].deadline` is added.
21. `GET /api/queues/{qname}` returns `history: []`, never `null`.
22. `taskInfo.result` is formatted with the task type passed to
    `ResultFormatter`. Upstream passed `""`.
23. `GET /api/redis_info`: the `info` map keeps values containing colons
    (`master0`, `db0`) and drops the `#` section headers.

### Ordering and stability

24. `GET /api/queues` and `GET /api/queue_stats` are sorted by queue name,
    and a queue deleted during the request is omitted instead of failing the
    whole call.
25. `scheduler_entries` are sorted by id, `servers` by host and pid, and
    `groups` by name.

### CLI and configuration

26. The CLI renders a non-UTF-8 payload through `SmartPayloadFormatter`
    instead of printing `"non-printable bytes"`, and the task DETAIL
    endpoint serves up to 262144 characters by default
    (`--max-detail-payload-length`).
27. The binary refuses to start with `--auth-header` set and
    `--trusted-proxies` empty. Pass `--allow-untrusted-auth-header` to keep
    the old permissive behaviour.
28. A Basic-Auth username is no longer the audit actor unless
    `--trust-basic-auth-user` is set or the peer is inside
    `--trusted-proxies`. Upstream and earlier fork builds accepted it
    unverified.
29. `--redis-cluster-nodes` forces `--disable-stats`,
    `--disable-error-index`, `--disable-hygiene` and `--disable-jobs` on.
    The fenced Lua batches behind those features touch keys the script does
    not declare in `KEYS`, which Redis Cluster rejects.
30. SIGTERM now drains for up to 10 seconds before the process exits. Set
    `terminationGracePeriodSeconds: 15` or more.

### Behaviour worth knowing

31. AQL: free text that contains `=`, `<`, `>` or `~` parses as a clause and
    must be quoted (`"user=42"`) to be searched literally. `died>` reads the
    archived score, so it also matches a task an operator archived by hand.
    `meta.KEY=` compares a numeric payload value numerically in float64, so
    an integer above 2^53 cannot be matched exactly.
32. A queue name containing `/` cannot be addressed by the API: the name
    matches a single URL path segment, so no route accepts it, not even
    percent-encoded. The console shows a notice and disables the mutating
    controls for such a queue.
33. The `observe` middleware now records only the `error` and `panic`
    outcomes and keeps records for 24 hours, not 7 days. Records written
    under the old defaults expire on their own.

## [Unreleased]

## [0.9.0] - 2026-09-08

Production-readiness review (tracking issue #57, child issues #27-#56). See
[docs/UPGRADING.md](docs/UPGRADING.md) before you move a deployment from
upstream 0.7.2, and
[Breaking and API changes since upstream 0.7.2](#breaking-and-api-changes-since-upstream-072)
for the API differences.

This is a minor release, not a patch, because the module path changed to
`github.com/zkrebbekx/asynqmon`. An embedder who used a `replace` directive
against the upstream path must import the new path instead. The binary and
the container image need no import change.

### Added

- (cmd): Graceful shutdown — `main` handled no signal, so `defer h.Close()` never ran and every SIGTERM left the stats, errsig and hygiene leases held for the rest of their 15s TTL. On SIGINT/SIGTERM the server now drains for up to 10s, then closes, then stops every engine while the Redis client is still open, so each role releases its lease at once. Set `terminationGracePeriodSeconds` to 15 or more (review #28)
- (cmd): `--redis-timeout` / `REDIS_TIMEOUT` (default 2s) sets the dial, read and write timeout of every Redis connection, plus a pool size of 20. go-redis applied its own 3s read timeout and ignored the caller's context, so `/healthz` answered at 3.0s against a hung Redis, past its own 2s budget (review #39)
- (cmd): `--max-concurrent-scans` / `MAX_CONCURRENT_SCANS` (default 4) and `--max-scan-ceiling` / `MAX_SCAN_CEILING` (default 20000, replacing the 100000 constant) bound the task-scan paths (review #27)
- (cmd): `--max-sse-connections` / `MAX_SSE_CONNECTIONS` (default 256) caps concurrent `/api/fleet/events` streams per replica; a negative value removes the cap (review #35)
- (cmd): `--trust-basic-auth-user` / `TRUST_BASIC_AUTH_USER` and `--allow-untrusted-auth-header` / `ALLOW_UNTRUSTED_AUTH_HEADER` govern identity trust (review #32)
- (cmd): `--hygiene-run-in-read-only` / `HYGIENE_RUN_IN_READ_ONLY` keeps `POST /api/hygiene/{kind}/run` available in read-only mode (review #53)
- (cmd): `--purge-owned-keys` / `PURGE_OWNED_KEYS` deletes every `asynqmon:*` key in the configured database, prints the counts and exits. It refuses and deletes nothing when a matched key falls outside the prefix, and it never touches an `asynq:*` key. This is the rollback path from the fork back to upstream (review #45)
- (cmd): `--version` prints the build version, which `-ldflags "-X main.version=..."` stamps and the Makefile fills from `git describe`. The startup line carries it and `GET /api/features` serves it as `version`; a running image could previously be identified only by its OCI revision label (review #46)
- (cmd): Nine flags for options only an embedder could reach — `--job-concurrency`, `--disable-jobs`, `--disable-error-index`, `--disable-hygiene`, `--hygiene-webhook-url`, `--attention-pending-age-slo`, `--attention-retry-storm-threshold`, `--attention-paused-long-after`, `--attention-group-stall-after` — each with its env var (review #43)
- (cmd): `INFO server` runs once at startup. It logs `redis_version` and warns below 6.2, the version `ZMSCORE`, streams, `MEMORY USAGE` and `HSTRLEN` need (review #37)
- (api): `page_size_applied` on every legacy list payload whose `size` the handler clamped. A script that dumped a queue with `?size=0` or `?size=50000` acted on a silent prefix (review #47)
- (api): `pending_since_unknown` on a task-scan response, and the AQL clause `pending_age=unknown`. asynq records `pending_since` on enqueue and scheduler forwarding only, so a task re-run with Run or Run all, or requeued by a worker shutdown, was silently excluded from `pending_age>` — exactly the query for stuck work (review #48)
- (api): `live_count` on every `GET /api/schedulers` row, and an "x2 registered" chip in the console. One process that registers the same entry twice collapsed into a single ordinary row while it enqueued the task twice per tick (review #54)
- (api): `POST /api/queues/{qname}/tasks` accepts an optional `headers` map and builds the task with `NewTaskWithHeaders`, so a clone-and-edit of a task that carries trace context keeps it. The Run-now route cannot: asynq stores a scheduler entry as spec, type, payload and option strings only (review #44)
- (api): `GET /api/health/roles` carries an `errsig` object with the error-signature indexer's last-run command spend — the total, the merge share, the allowance and the finish time. The merge phase and the retry sample were previously missing from `spent`, so the health readout understated the indexer's Redis cost (review #52)
- (ui): A non-blocking notice on a queue whose name contains `/`, with the mutating controls disabled. Such a name matches no API route, not even percent-encoded (review #49)
- (observe): `WithSampling(rate)` records a stable per-task fraction, and `WithOutcomes(...)` selects the recorded outcomes. Every recorded attempt increments `asynqmon:obs:count:<YYYY-MM-DD>` (48h TTL) so a reader can show the footprint without a `SCAN` (review #38)
- (docs): `docs/UPGRADING.md` — the required Redis ACL, the key inventory with TTLs, the expected memory, the replica and read-only semantics, the deployment settings, the asynq 0.25 queue-publish caveat, and the rollback recipe (review #45)
- (docs): A "Breaking and API changes since upstream 0.7.2" section in this file, and README sections for Requirements, limits and status codes (review #37, #45, #47)
- (ui): The task drawer decodes base64-encoded payload/result fields by default, with a decoded/raw toggle and an honesty note naming exactly which fields were transformed. Detection is layered to make false positives statistically negligible (strict charset/length shape, a character-class guard that rejects slugs and single-class strings, and a strict fully-printable-UTF-8 decode check that rejects hex strings, dashless UUIDs, and binary blobs); base64-of-JSON embeds as structure, whole-payload base64 is handled, and copy always copies raw

### Changed

- (build): The module path is `github.com/zkrebbekx/asynqmon`. It stayed at `github.com/hibiken/asynqmon`, so `go install` of this fork failed and the README import paths resolved to upstream (review #42)
- (build): `go.mod` moves to `go 1.26.0` with `toolchain go1.26.8`; CI and the release binaries built on the EOL Go 1.21.13 before (review #29)
- (deps): asynq 0.24.1 to 0.26.0, go-redis 9.0.4 to 9.22.0 (clears GO-2025-3540), client_golang 1.24.1, mux 1.8.1, go-cmp 0.7.0. The supported asynq range is now 0.24.x - 0.26.x (review #30, #44)
- (search): The scan endpoints stream each match into a sink instead of holding every formatted row in memory. One `GET /api/tasks?max_scan=100000` built up to 100k rows before paging (review #27)
- (search): `POST /api/tasks:batch_filtered` collects at most 2000 matches and answers 400 with a pointer to `POST /api/jobs` beyond that. Its per-request ticker becomes one package-level 1000/s limiter shared by every request; the synchronous path could otherwise run past the 10s write timeout while the mutation completed (review #31)
- (search): `meta.KEY=` compares numbers numerically (10 = 10.0 = 1e1) on both the legacy filter and the AQL clause (review #54)
- (search): The legacy scan reports `truncated=true` only when the budget stopped it with work left, not when the last batch ended exactly at the budget (review #54)
- (api): `state_counts` answers 404 for a queue missing from `asynq:queues` only when every per-state count is zero, so an asynq 0.25+ producer's cached publish cannot hide a refilled queue (review #44)
- (api): `POST /api/hygiene/{kind}/run` answers 429 with `Retry-After` when the persisted report is younger than 30s, and sits behind the read-only filter by default. A refused run writes no report and no audit entry (review #53)
- (api): `POST /api/queues/{qname}/tasks` refuses a queue name `Inspector.Queues()` does not list, unless the body carries `"create_queue": true`, and refuses a deadline that is not in the future (review #53)
- (api): CORS `AllowedMethods` gains `PUT`, which the views and hygiene-config routes need, and the same-origin check accepts `X-Forwarded-Host` when the peer is inside `--trusted-proxies` (review #53)
- (cmd): `--redis-cluster-nodes` forces `--disable-stats`, `--disable-error-index`, `--disable-hygiene` and `--disable-jobs` on and logs one line. The fenced Lua batches behind those features call `redis.call` on keys the script does not declare in `KEYS`, which Redis Cluster rejects (review #37)
- (cmd): `--redis-password` now applies after `ParseRedisURI` when the URL carried no password, so `REDIS_URL` from a secret plus `REDIS_PASSWORD` no longer fails with `NOAUTH` (review #43)
- (stats): `DefaultSchedulerGoneAfter` derives from the asynq heartbeat contract — the entry TTL plus two heartbeats, 40s on asynq 0.25+, up from 30s — so a 0.26 scheduler is not flagged GONE too early (review #44)
- (stats): Server-scope series rings expire after 7 days instead of 60, and the sweep unlinks the ring keys of a server absent for over 24 hours. A daily redeploy of five pods left about 1.8 MB of dead keys resident (review #52)
- (stats): `readGroupStalls` samples the group set with `SRANDMEMBER` instead of reading all of it with `SMEMBERS` (review #52)
- (observe): The default TTL is 24 hours, not 7 days, and the middleware records only the `error` and `panic` outcomes by default. It wrote about 750 B per task for 7 days with no global bound; 200k tasks a day cost about 1 GB on the shared queue Redis (review #38)
- (ui): recharts (508 kB) leaves the eager entry graph. It was preloaded by `index.html` on every route although only the lazy Metrics view uses it; the eager payload drops from 777 kB to 426 kB (review #50)
- (docs): README screenshots regenerated against the current console — the previous set predated the Fleet Console rebuild entirely; adds an Overview screenshot (the landing screen had none) and content-fit captures of the Queues directory, queue workspace, Tasks console, Metrics, and dark-mode Settings
- (docs): Install/run instructions point at this fork's own releases page and `ghcr.io/zkrebbekx/asynqmon` images instead of upstream's, which include none of the fork's console
- (docs): README documents the task drawer's base64 payload decoding (detection layers, decoded/raw toggle, copy-copies-raw) with a live-app screenshot

### Fixed

- (all): A panic on a background goroutine no longer ends the process. net/http recovers only on the handler goroutine, and this binary runs nineteen other goroutines: the lease loops of the sweeper, the error indexer, the hygiene scheduler and the bulk-job runner, the SSE publish and relay loops, the queue and task fan-outs, the metrics fetches, the view seeder and the webhook delivery. Every one now runs through the new `internal/safego` package, which recovers, logs the stack, and restarts a lease loop with a 100ms-to-5s backoff. A panicking HTTP handler answers `500 {"error":"internal error"}` instead of dropping the connection. A test parses every non-test file and fails on a bare `go` statement (review #53)

- (handler): `New` no longer performs synchronous Redis writes before it returns. Seeding the five system views on the constructor path blackholed the listener for about 10s with Redis unreachable, so a restarted pod failed its TCP liveness check for that whole window while the SPA, which needs no Redis, was unavailable with it. The seeding now runs in the background with a 3s context per attempt and a 1s-to-30s backoff (review #41)
- (stats): A sweep no longer aborts on the first failed write. The tick publishes to memory first and writes after, so a Redis memory incident no longer stops the shared cache and no longer makes standby replicas answer 503 after the 2-minute cache TTL. A standby now serves its last cache read as `"source": "stale-cache"` with the original `refreshed_at`. Repeating sweep errors collapse to one line, then one per minute, then a recovery line (review #36)
- (stats): `Engine.Stop` flushes the queued slots and the partial accumulators through the fence before it releases the lease (review #28)
- (stats): A queue is marked viewed only after a 2xx. An unauthenticated `GET /api/queues/{qname}` wrote the requested name into the shared `asynqmon:viewed` zset before the handler ran, so any client on the allowed network grew a key in the production Redis by one member per request. The zset is capped at 4000 members and the in-process throttle map at 4096 entries (review #33)
- (leasefence): The fence token is minted as `<redis TIME seconds>:<sequence>` inside the acquire script. A bare `INCR` counter restarts at 1 after a Redis data loss, so a paused ex-holder could pass the compare-and-set with the same token as the new holder. A fenced write now also requires current possession of the lock, and the stand-down after a rejected write is skipped when the instance re-acquired the lease mid-run (review #52)
- (leasefence): `Exec` splits `SADD`, `SREM`, `HSET`, `HDEL`, `DEL`, `UNLINK`, `RPUSH`, `LPUSH`, `ZADD` and `ZREM` above 1000 members before the `EVAL`. Redis Lua 5.1 `unpack` fails at about 8000 arguments, which the cache queue set and the errsig cursor hash reach above 8000 queues or signatures (review #52)
- (jobs): The bulk-job execute loop checks the claim and the context per task instead of per batch, renews the claim before every batch, and classifies "task is already <state>" as skipped instead of failed. A holder that stalled past its lease acted on the same 100-task batch as its successor and the loser's errors counted as failures (review #40)
- (jobs): `progressScript` sets the TTLs of the candidates, sample and failures keys inside the same atomic call. A runner that died between the Lua write and the trailing best-effort pipeline left those keys with no TTL until the job was reclaimed (review #45)
- (errsig): The archived-tail sweep stops one safety lag (2s by default) short of the current second. asynq scores the archived zset in whole seconds, so a task archived later in the same second as the cursor, whose id sorted below the cursor id, was skipped forever
- (aql): `died>` reads the archived score in every execution mode. `Inspector.ArchiveTask` leaves `LastFailedAt` zero, so a preview built in cursor mode shrank when the bulk-job runner re-verified it with the predicate (review #54)
- (aql): Every rejection of a clause-shaped token now ends with the advice `to search this text literally, quote it: "user=42"`. `q=user=42` answered 400 "unknown field `user`" and never said how to search the text (review #54)
- (scheduler): A Run-now `Deadline` whose zone abbreviation the host cannot resolve is skipped with a reason instead of applied at a zero offset. `time.ParseInLocation` accepts an unresolvable abbreviation without an error, so `Mon Sep 7 22:00:00 AEST 2026` became 22:00:00 +0000 under `TZ=UTC` — a deadline 10 hours off, reported as applied. A deadline already in the past is skipped too (review #54)
- (sse): Every `/api/fleet/events` write runs with a 30s deadline through `http.ResponseController`, so a half-open peer is dropped in 30s instead of blocking the handler goroutine. The on-connect jobs snapshot comes from a broker cache refreshed at most every 30s, so N reconnects cost one Redis read instead of N. The heartbeat also emits a named `ping` event every 15s, which `EventSource` can see (review #35)
- (search): `scanMatchingTasks`, `execScanPlan` and the cursor fan-out check `ctx.Err()` per page, so a disconnected client stops the scan (review #39)
- (ui): Every API path segment is escaped — the queue name, the group name, the task id, the job id, the scheduler key, the error signature, the view id and the hygiene kind. A queue named `a#b` retargeted destructive requests (review #49)
- (ui): axios carries a request timeout, `usePolling` guards against overlapping ticks, and the jobs stream has a liveness watchdog that counts the server's 15s ping and falls back to polling after 45s of silence. `ErrorsView`, `SchedulersView`, `HygieneView` and `ServersView` discard an out-of-order response (review #51)
- (api): The task-metadata facet sampler drops high-cardinality payload keys (UUID-ish ids whose values are all distinct) instead of flooding the console's chip row with pages of count-1 chips that cannot drill anything down

### Security

- (identity): The audit actor is no longer forgeable through an unverified Basic-Auth username. `resolve()` accepts the username only when `--trust-basic-auth-user` is set or the peer is inside a non-empty `--trusted-proxies` set. Any client could otherwise attribute an audit entry to a colleague and satisfy `--require-identity`. Behind a trusted proxy the anonymous fallback now names the client IP from `X-Forwarded-For` instead of the proxy address (review #32)
- (cmd): The binary refuses to start with `--auth-header` set and `--trusted-proxies` empty; `--allow-untrusted-auth-header` acknowledges the risk deliberately (review #32)
- (api): A 5xx body is generic. The API echoed the raw error on every 5xx, so a Redis outage returned `dial tcp 10.x.x.x:6379: ...` and a missing Prometheus returned the full query URL to an unauthenticated dashboard. The full error goes to the process log (review #53)
- (static): The SPA and the API send `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin` and a Content-Security-Policy with `frame-ancestors 'none'`. The dashboard has no login of its own and renders untrusted task payloads, so any page an operator visited could frame it and overlay a destructive action. The one inline bootstrap script carries a fresh 128-bit nonce per render, so the CSP keeps `script-src` (review #34)
- (cmd): The Redis password stays out of fatal error messages. `runPurge` returns a static error, and a failed URL parse goes through `redactURIError`, which replaces the userinfo password with `***` and keeps the host and the reason readable (review #57)
- (deps): go-redis 9.22.0 clears GO-2025-3540 on the connection path (review #30)

## [0.8.0] - 2026-08-22

First tagged release of this fork ("Fleet Console"): the full console rebuild
(Overview/attention engine, Queues directory, AQL task search, Errors,
Operations bulk jobs + audit, Hygiene, saved views, SSE live updates) plus the
2026-08-20 full-review hardening pass below. Requires asynq 0.24.x.

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
- (pkg/cmd): `GET /healthz` liveness/readiness probe — PINGs redis with a 2s budget, 200/503 JSON (upstream hibiken/asynqmon#276)
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
