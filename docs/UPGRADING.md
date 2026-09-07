# Upgrading to this fork

This guide is for an operator who runs upstream `hibiken/asynqmon` 0.7.2 and
wants to run this fork instead. Upstream 0.7.2 is a stateless reader: it
writes nothing to your Redis. This fork is not. It keeps its own state under
the `asynqmon:` key prefix in the same Redis database as your queues, it runs
background workers, and it elects one replica per background role.

Read this page before you change the image tag.

## Contents

1. [What changes on day one](#what-changes-on-day-one)
2. [Redis requirements](#redis-requirements)
3. [The Redis ACL the fork needs](#the-redis-acl-the-fork-needs)
4. [What the fork writes to your Redis](#what-the-fork-writes-to-your-redis)
5. [Expected memory](#expected-memory)
6. [Replicas and background roles](#replicas-and-background-roles)
7. [Read-only mode](#read-only-mode)
8. [Deployment settings to change](#deployment-settings-to-change)
9. [asynq 0.25 and later: the queue-publish cache](#asynq-025-and-later-the-queue-publish-cache)
10. [Rollback](#rollback)
11. [Pre-flight checklist](#pre-flight-checklist)

## What changes on day one

| Area | Upstream 0.7.2 | This fork |
| --- | --- | --- |
| Redis writes | none | keys under `asynqmon:` in the same database |
| Redis ACL | read is enough | write on `asynqmon:*` is required |
| Background work | none | stats sweeper, error index, hygiene reports, bulk jobs |
| Replicas | all equal | one replica holds each background role through a lease |
| Redis version | any | 6.2 or later |
| Redis Cluster | reads work | reads work; the write features are forced off |
| Error bodies | `text/plain` | `application/json`, generic for 5xx |
| Shutdown | immediate | graceful, up to 10 seconds |

The API changes are listed in
[CHANGELOG.md](../CHANGELOG.md#breaking-and-api-changes-since-upstream-072).

## Redis requirements

Redis **6.2 or later**. The console uses `ZMSCORE` (task search and AQL),
Redis streams (the audit log), `MEMORY USAGE` and `HSTRLEN` (the Hygiene
storage report). The binary runs `INFO server` once at startup, logs
`redis: server version X`, and prints a `WARNING` line when the server is
below 6.2.

**Redis Cluster is not supported for the write features.** The fenced Lua
batches call `redis.call` on keys that the script does not declare in `KEYS`,
which Redis Cluster rejects. Setting `--redis-cluster-nodes` therefore forces
`--disable-stats`, `--disable-error-index`, `--disable-hygiene` and
`--disable-jobs` on, and the binary logs that once. The Overview, Errors,
Hygiene and Operations screens are then unavailable. Use a single instance or
Sentinel if you want them.

## The Redis ACL the fork needs

A Redis user that can only read cannot run this fork. Each background role
takes its lease with `SET NX PX`. A read-only user never acquires one, so
`/api/fleet/*` answers 503 for as long as the process runs and the log
repeats `acquiring sweeper lease: NOPERM` every few seconds.

Minimum user:

```
ACL SETUSER asynqmon on >CHANGEME \
  ~asynq:* ~asynqmon:* \
  &asynqmon:events:jobs &asynq:cancel \
  +@read +@write +@keyspace +@scripting +@transaction +@connection \
  +@pubsub +info +memory|usage
```

On Redis 7.0 or later you can keep the queue data read-only and still let the
console own its own keys:

```
ACL SETUSER asynqmon on >CHANGEME \
  %R~asynq:* %RW~asynqmon:* \
  &asynqmon:events:jobs &asynq:cancel \
  +@read +@write +@keyspace +@scripting +@transaction +@connection \
  +@pubsub +info +memory|usage
```

That second form still lets the console read every queue, but it cannot
delete a task, pause a queue or enqueue. Pair it with `--read-only`.

Why each permission is needed:

| Permission | Used by |
| --- | --- |
| `~asynq:*` | every read of your queue data; the mutations, unless the user is read-only |
| `~asynqmon:*` | the fork's own state (see the next section) |
| `+@scripting` | the fenced Lua batches of the stats sweeper, the error index, the hygiene scheduler and the bulk-job runner |
| `+@pubsub`, `&asynqmon:events:jobs` | the SSE stream that pushes bulk-job progress between replicas |
| `&asynq:cancel` | asynq's own cancelation channel, used by "cancel active task" |
| `+info` | the startup version probe and `GET /api/redis_info` |
| `+memory\|usage` | the Hygiene storage report |

Pass the user with `--redis-username` / `REDIS_USERNAME`.

## What the fork writes to your Redis

Every key starts with `asynqmon:`. Nothing outside that prefix is created by
the fork.

### Stats sweeper

| Key | Type | TTL | Other bound |
| --- | --- | --- | --- |
| `asynqmon:cache:q:<queue>` | HASH | `max(10 x --stats-interval, 2m)` | — |
| `asynqmon:cache:fleet` | HASH | the same | — |
| `asynqmon:cache:queues` | SET | the same | — |
| `asynqmon:cache:attention` | STRING | the same | — |
| `asynqmon:viewed` | ZSET | 5 minutes | trimmed to 4000 members |
| `asynqmon:sched` | ZSET | **none** | pruned 30 days after an entry was last seen |
| `asynqmon:sched:<stable_key>` | HASH | **none** | the same 30-day prune |
| `asynqmon:series:h:<scope>:<metric>` | STRING | 6 hours | fixed 1452 bytes per key |
| `asynqmon:series:r:<scope>:<metric>` | STRING | 60 days, 7 days for a server scope | fixed 2892 bytes per key; a server absent for 24 hours is unlinked |
| `asynqmon:series:since` | STRING | **none** | one key, written once |

### Error-signature index

Every key below carries a 90-day TTL, refreshed on each write.

| Key | Type | Other bound |
| --- | --- | --- |
| `asynqmon:idx:err` | ZSET | at most 1000 signatures |
| `asynqmon:idx:err:<sig>` | HASH | evicted with the index cap |
| `asynqmon:idx:err:<sig>:refs` | ZSET | at most 100 task refs |
| `asynqmon:idx:err:cursors` | HASH | one field per queue |
| `asynqmon:idx:err:meta` | HASH | a fixed field set |
| `asynqmon:idx:err:retrysigs` | SET | rewritten on each sweep |

### Hygiene

| Key | Type | TTL | Other bound |
| --- | --- | --- | --- |
| `asynqmon:hygiene:report:<kind>` | STRING | 90 days | four kinds |
| `asynqmon:hygiene:lastgen` | HASH | 90 days | four fields |
| `asynqmon:hygiene:config` | HASH | **none** | one field per kind |

### Bulk jobs and the audit log

| Key | Type | TTL | Other bound |
| --- | --- | --- | --- |
| `asynqmon:jobs` | ZSET | **none** | trimmed to 1000 jobs |
| `asynqmon:jobs:<id>` | HASH | 30 days | — |
| `asynqmon:jobs:<id>:candidates` | LIST | 30 days | at most 1,000,000 refs; the job fails past that |
| `asynqmon:jobs:<id>:sample` | LIST | 30 days | 10 entries |
| `asynqmon:jobs:<id>:failures` | LIST | 30 days | 10,000 entries |
| `asynqmon:jobs:<id>:lease` | STRING | 15 seconds | — |
| `asynqmon:audit` | STREAM | **none** | `MAXLEN ~ 10000` |

### Saved views, markers and leases

| Key | Type | TTL | Other bound |
| --- | --- | --- | --- |
| `asynqmon:views:index` | ZSET | **none** | at most 500 views |
| `asynqmon:views:<id>` | HASH | **none** | the same cap; 4096 bytes of state per view |
| `asynqmon:markers` | ZSET | **none** | 90-day and 1000-entry trim on write |
| `asynqmon:lock:<role>` | STRING | 15 seconds | three roles: `stats`, `errsig`, `hygiene` |
| `asynqmon:lock:<role>:fence` | STRING | **none** | three small keys; the TTL is left off on purpose, because expiring the fence while a holder still presents a token minted from it would break the fencing guarantee |

### Written by your workers, not by asynqmon

The optional `observe` middleware runs inside your worker processes. If you
adopt it, the workers write:

| Key | Type | TTL |
| --- | --- | --- |
| `asynqmon:obs:att:<queue>:<task_id>` | LIST | 24 hours, 30 attempts |
| `asynqmon:obs:sum:<queue>:<task_id>` | HASH | 24 hours |
| `asynqmon:obs:count:<YYYY-MM-DD>` | STRING | 48 hours |

## Expected memory

The measured figures, from `docs/SCALE.md` and the package comments:

| Item | Size |
| --- | --- |
| Fleet cache, per queue | about 392 bytes |
| Fleet cache, 5000 queues | about 2.23 MB |
| Hot series ring, per queue and metric | 1452 bytes, 6-hour TTL |
| Rollup series ring, per queue and metric | 2892 bytes, 60-day TTL |
| Audit stream, full | up to about 4 MB (10,000 entries) |
| Bulk-job preview | about 40-60 bytes per candidate; up to about 60 MB for a job at the 1M cap, for 30 days |
| `observe` middleware, per recorded task | about 750 bytes |

A fleet of 10 queues over 30 days sits in the 1-6 MB range for the background
state. The two items that can grow far past that are the bulk-job previews
and the `observe` middleware. Size them before you enable either:

```
observe footprint = recorded_tasks_per_day x TTL_in_days x 750 B
```

At the defaults the middleware records only the `error` and `panic`
outcomes, so `recorded_tasks_per_day` is your failure rate, not your
throughput. Use `observe.WithSampling(rate)` to cut it further, and
`observe.WithTTL` to shorten the 24-hour retention. Read
`asynqmon:obs:count:<YYYY-MM-DD>` for the real daily figure.

## Replicas and background roles

The fork has three leased background roles: `stats` (the fleet sweeper),
`errsig` (the error-signature indexer) and `hygiene` (the report scheduler).
Each lease is one `SET NX PX` key with a 15-second TTL and a fencing token.
Exactly one replica holds each role at a time; the others serve the shared
cache that the holder publishes.

What this means for you:

- Every replica still serves the whole API. There is no leader-only endpoint.
- A replica that holds no role still writes: `asynqmon:viewed`, saved views,
  markers and audit entries are written by whichever replica served the
  request.
- A fourth background worker, the bulk-job runner, is **not** leased. It
  claims individual jobs with a per-job lease, so several replicas work
  different jobs at the same time. Bound it with `--job-concurrency`
  (default 2 jobs per replica).
- `GET /api/health/roles` reports which instance holds each role.
- During a Redis write outage the holder keeps serving from memory, and a
  standby serves its last cache read with `coverage.source` set to
  `"stale-cache"` instead of answering 503.

Turn a role off per replica with `--disable-stats`, `--disable-error-index`,
`--disable-hygiene` or `--disable-jobs`. The matching read endpoints keep
working; another replica does the writing.

## Read-only mode

`--read-only` blocks every mutating API route. It does **not** turn the
fork into a stateless reader:

- The stats sweeper, the error-signature indexer and the hygiene report
  generator keep running, and keep writing `asynqmon:*` keys.
- The bulk-job runner does not start.
- `POST /api/hygiene/{kind}/run` is blocked by default. Set
  `--hygiene-run-in-read-only` to keep it available.

A read-only deployment therefore still needs write access to `asynqmon:*`.
Combine `--read-only` with the Redis 7 ACL above to keep `asynq:*` read-only.

## Deployment settings to change

**Termination grace period.** The binary now handles SIGTERM. It drains
in-flight requests for up to 10 seconds, then closes the listener and stops
every engine, which releases the three role leases at once. Give it room:

```yaml
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 15
```

With a shorter period, or with the SIGKILL that Kubernetes sends at the end
of it, the leases stay held for the rest of their 15-second TTL and no
replacement replica picks up the role in that window.

**Probes.** Point the readiness probe at `GET /healthz`. Keep the liveness
probe as a TCP check on the HTTP port. An HTTP liveness probe on `/healthz`
restart-loops the pods during a Redis outage, and a restart cannot fix Redis.
The bundled Helm chart is configured this way.

**Redis timeout.** `--redis-timeout` (default 2s) sets the dial, read and
write timeout of every Redis connection. Keep it at or below the 2-second
`/healthz` budget.

**Identity.** If you set `--auth-header`, you must also set
`--trusted-proxies`, or the binary refuses to start. A Basic-Auth username is
no longer accepted as the audit actor unless `--trust-basic-auth-user` is set
or the peer is a trusted proxy.

## asynq 0.25 and later: the queue-publish cache

asynq 0.25.0 caches "this queue is already registered" per client process.
If an operator deletes an empty queue through asynqmon while a 0.25-or-later
producer keeps enqueuing into it, that producer never re-adds the queue name
to `asynq:queues`. `Inspector.Queues()` then reports the queue as gone while
tasks keep accumulating in it.

This fork answers `404` on `state_counts` for such a queue only when every
per-state count is zero, so a refilled queue stays visible. The underlying
asynq behaviour is unchanged: restart the producer after you delete a queue
it uses, or do not delete queues that still have producers.

## Rollback

To go back to upstream 0.7.2, or to any build that does not use the
`asynqmon:` prefix, remove the fork's keys. Otherwise they sit in your Redis
with no reader: the no-TTL keys in the tables above (`asynqmon:sched*`,
`asynqmon:views:*`, `asynqmon:markers`, `asynqmon:jobs`, `asynqmon:audit`,
`asynqmon:hygiene:config`, `asynqmon:series:since` and the three fence keys)
never expire on their own.

1. Scale the fork to zero replicas. Do not run the purge while a replica is
   writing.
2. Run the purge with the same connection flags the deployment uses:

   ```bash
   asynqmon --redis-url=redis://... --purge-owned-keys
   ```

   The helper runs `SCAN 0 MATCH asynqmon:* COUNT 500` to completion, checks
   that every matched key starts with `asynqmon:`, `UNLINK`s the keys in
   batches of 500, prints the counts and exits. It never touches an
   `asynq:*` key. If any matched key falls outside the prefix, it deletes
   nothing and exits with an error.
3. Deploy the old image.

The purge deletes your saved views, markers, hygiene configuration and the
audit log along with the caches. Export anything you want to keep first:

```bash
redis-cli --scan --pattern 'asynqmon:views:*'
redis-cli XRANGE asynqmon:audit - +
```

Your queue data is untouched by all of this. Rolling back does not lose a
task.

## Pre-flight checklist

- [ ] Redis is 6.2 or later, and it is not a cluster (or you accept the
      read-only console).
- [ ] The Redis user has write access to `asynqmon:*`, scripting, and pub/sub.
- [ ] You have budgeted the memory in [Expected memory](#expected-memory) on
      the queue Redis.
- [ ] `terminationGracePeriodSeconds` is 15 or more.
- [ ] Readiness probes `/healthz`; liveness is a TCP check.
- [ ] `--auth-header` is paired with `--trusted-proxies`.
- [ ] You have read the API changes in
      [CHANGELOG.md](../CHANGELOG.md#breaking-and-api-changes-since-upstream-072).
- [ ] You know the rollback command:
      `asynqmon --purge-owned-keys`.
