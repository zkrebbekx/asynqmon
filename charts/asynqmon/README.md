# asynqmon Helm chart

Deploys [asynqmon](https://github.com/zkrebbekx/asynqmon) — the Web UI for the
[Asynq](https://github.com/hibiken/asynq) task queue.

## Install

```bash
helm install asynqmon ./charts/asynqmon \
  --set image.repository=ghcr.io/zkrebbekx/asynqmon \
  --set redis.url=redis://my-redis:6379
```

The image is published by this repo's GitHub Actions workflow to
`ghcr.io/zkrebbekx/asynqmon` on every push to `master` and on tags.

## Common configurations

Single Redis with a password from an existing secret:

```bash
helm install asynqmon ./charts/asynqmon \
  --set redis.addr=my-redis:6379 \
  --set redis.existingSecret=redis-creds \
  --set redis.existingSecretPasswordKey=password
```

Redis Cluster:

```bash
helm install asynqmon ./charts/asynqmon \
  --set redis.clusterNodes="node1:6379\,node2:6379\,node3:6379"
```

Expose via Ingress + scrape metrics with Prometheus Operator:

```bash
helm install asynqmon ./charts/asynqmon \
  --set redis.url=redis://my-redis:6379 \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=asynqmon.example.com \
  --set prometheus.enableExporter=true \
  --set serviceMonitor.enabled=true
```

## Redis credentials

`redis.url` renders as a plain environment variable in the pod spec, so anyone
with `get deployment` reads the password inside it. Keep the URL in a secret
instead:

```bash
kubectl create secret generic redis-creds \
  --from-literal=url='rediss://user:s3cret@my-redis:6379/0'

helm install asynqmon ./charts/asynqmon \
  --set redis.existingSecret=redis-creds \
  --set redis.existingSecretUrlKey=url
```

The binary builds the whole connection from the URL. It never applies
`REDIS_PASSWORD` in that mode, so the chart refuses to render `redis.url`
together with `redis.password` or `redis.existingSecret`. Choose one of:

| Mode | Values |
| --- | --- |
| URL in a secret (preferred) | `redis.existingSecret`, `redis.existingSecretUrlKey` |
| URL inline | `redis.url` |
| Host and port | `redis.addr`, `redis.db`, `redis.password` or `redis.existingSecret` |
| Cluster | `redis.clusterNodes`, `redis.password` or `redis.existingSecret` |

Every other secret (`redis.sentinelPassword`, `prometheus.basicAuth`,
`hygiene.webhookUrl`) reaches the container as an environment variable from a
secret, never as a command-line argument. Arguments are visible in
`kubectl get pod` output and in the process argv.

## Running more than one replica

Every background role is leased: the fleet stats sweeper, the hygiene engine,
the job runner and the error-signature indexer each take a Redis lease, and
only the lease holder runs. The other replicas serve the Web UI and the API,
and take over within one lease period when the holder dies. Running
`replicaCount > 1` therefore adds read capacity and removes the single point
of failure. It does not multiply the background work, and it does not need
sticky sessions.

With `replicaCount > 1`, set `pdb.enabled=true` so a node drain cannot take
every replica at once, and set `topologySpreadConstraints` to spread the
replicas over zones or nodes. Keep `pdb.enabled=false` with a single replica:
`minAvailable: 1` would block every voluntary drain.

## Probes

Readiness calls `GET /healthz`, which PINGs Redis with a 2s budget and answers
`200` or `503`. `readinessProbe.timeoutSeconds` is 4 so the kubelet waits
longer than that budget; a shorter timeout makes readiness flap on any Redis
stall. Liveness is a TCP check on the HTTP port, on purpose: an HTTP liveness
probe on `/healthz` restart-loops the pods during a Redis outage, and a
restart cannot fix Redis.

## Values

`""` in the default column means "keep the binary's own default". The chart
does not repeat the default, so the two cannot drift.

### Image and deployment

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `replicaCount` | | Number of pods | `1` |
| `image.repository` | | Image repo | `ghcr.io/zkrebbekx/asynqmon` |
| `image.tag` | | Image tag | chart `appVersion` |
| `image.pullPolicy` | | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | | Pull secrets | `[]` |
| `resources` | | Container resources | `50m`/`128Mi` requests, `256Mi` memory limit |
| `goMemLimit` | | `GOMEMLIMIT` env; keep it under `resources.limits.memory` | `200MiB` |
| `pdb.enabled` | | Create a PodDisruptionBudget | `false` |
| `pdb.minAvailable` | | PDB `minAvailable` | `1` |
| `pdb.maxUnavailable` | | PDB `maxUnavailable` (exclusive with `minAvailable`) | `""` |
| `topologySpreadConstraints` | | Pod spread constraints | `[]` |
| `automountServiceAccountToken` | | Mount the API token (asynqmon calls no Kubernetes API) | `false` |
| `podSecurityContext` | | Pod security context, incl. `seccompProfile: RuntimeDefault` | see `values.yaml` |
| `securityContext` | | Container security context | see `values.yaml` |
| `livenessProbe` / `readinessProbe` | | Probes; see above | see `values.yaml` |
| `nodeSelector`, `tolerations`, `affinity` | | Scheduling | `{}` / `[]` / `{}` |
| `serviceAccount.create` / `.name` / `.annotations` | | ServiceAccount | `true` / `""` / `{}` |
| `service.type` / `.port` / `.annotations` | | Service | `ClusterIP` / `8080` / `{}` |
| `ingress.*` | | Ingress | disabled |
| `serviceMonitor.*` | | Prometheus-Operator ServiceMonitor (needs `prometheus.enableExporter`) | disabled |
| `extraArgs` | | Extra raw flags | `[]` |
| `extraEnv` | | Extra environment variables | `[]` |
| `podAnnotations` / `podLabels` | | Pod metadata | `{}` |

The container always receives `--port=8080`; change the published port with
`service.port`.

### Redis

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `redis.url` | `--redis-url` | Connection URL (`redis://`, `rediss://`, `redis-sentinel://`) | `""` |
| `redis.addr` | `--redis-addr` | `host:port` | `""` |
| `redis.db` | `--redis-db` | Database number | `0` |
| `redis.clusterNodes` | `--redis-cluster-nodes` | Comma-separated cluster nodes | `""` |
| `redis.password` | | `REDIS_PASSWORD` (inline; prefer a secret) | `""` |
| `redis.username` | | `REDIS_USERNAME`, the Redis 6 ACL user | `""` |
| `redis.sentinelPassword` | | `REDIS_SENTINEL_PASSWORD` for the sentinel nodes | `""` |
| `redis.tls` | `--redis-tls` | Server name for TLS validation | `""` |
| `redis.insecureTLS` | `--redis-insecure-tls` | Skip TLS host checks | `false` |
| `redis.timeout` | `--redis-timeout` | Dial/read/write timeout (binary default `2s`) | `""` |
| `redis.existingSecret` | | Secret holding the redis credentials | `""` |
| `redis.existingSecretPasswordKey` | | Key holding the password | `redis-password` |
| `redis.existingSecretUrlKey` | | Key holding the whole URL; use instead of `redis.url` | `""` |
| `redis.existingSecretSentinelPasswordKey` | | Key holding the sentinel password | `""` |

### Access control and API surface

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `readOnly` | `--read-only` | Read-only Web UI | `false` |
| `enableEnqueue` | `--enable-enqueue` | Allow creating tasks from the Web UI | `false` |
| `purgeOwnedKeys` | `--purge-owned-keys` | Let maintenance actions delete keys asynqmon owns | `false` |
| `auth.header` | `--auth-header` | Reverse-proxy header resolved as the acting user | `""` |
| `auth.trustedProxies` | `--trusted-proxies` | CIDRs the auth header is trusted from | `""` |
| `auth.requireIdentity` | `--require-identity` | Refuse mutating requests without an identity | `false` |
| `auth.trustBasicAuthUser` | `--trust-basic-auth-user` | Accept the Basic-auth user as the identity | `false` |
| `auth.allowUntrustedAuthHeader` | `--allow-untrusted-auth-header` | Accept the auth header from any peer | `false` |
| `corsAllowedOrigins` | `--cors-allowed-origins` | Allowed cross-origin origins (empty: same-origin only) | `""` |

### Web UI limits

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `ui.maxPayloadLength` | `--max-payload-length` | Payload characters in list cells (binary default `200`) | `""` |
| `ui.maxResultLength` | `--max-result-length` | Result characters in list cells (binary default `200`) | `""` |
| `ui.maxDetailPayloadLength` | `--max-detail-payload-length` | Payload characters on the task detail endpoint (binary default `262144`, `0` = unlimited) | `""` |
| `ui.correlationKeys` | `--correlation-keys` | Payload keys the Flow view follows (binary default `trace_id,correlation_id,request_id`) | `""` |

### Metrics

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `prometheus.enableExporter` | `--enable-metrics-exporter` | Serve `/metrics` | `false` |
| `prometheus.address` | `--prometheus-addr` | Prometheus server queried by the metrics view | `""` |
| `prometheus.basicAuth` | | `user:password` sent with those queries | `""` |
| `prometheus.existingSecret` | | Secret holding the basic-auth credentials | `""` |
| `prometheus.existingSecretBasicAuthKey` | | Key inside that secret | `prometheus-basic-auth` |

### Background roles

| Key | Flag | Description | Default |
| --- | --- | --- | --- |
| `stats.interval` | `--stats-interval` | Fleet stats sweep interval (binary default `5s`) | `""` |
| `stats.disabled` | `--disable-stats` | Disable the sweeper and `/api/fleet` | `false` |
| `scans.maxConcurrent` | `--max-concurrent-scans` | Concurrent task scans (binary default `4`) | `""` |
| `scans.maxCeiling` | `--max-scan-ceiling` | Hard cap on tasks scanned per request (binary default `20000`) | `""` |
| `scans.maxSSEConnections` | `--max-sse-connections` | Concurrent SSE streams (binary default `256`) | `""` |
| `jobs.disabled` | `--disable-jobs` | Disable the background job runner | `false` |
| `jobs.concurrency` | `--job-concurrency` | Jobs run at once (binary default `2`) | `""` |
| `errorIndex.disabled` | `--disable-error-index` | Disable the error-signature index | `false` |
| `hygiene.disabled` | `--disable-hygiene` | Disable the queue hygiene engine | `false` |
| `hygiene.runInReadOnly` | `--hygiene-run-in-read-only` | Run hygiene actions in read-only mode | `false` |
| `hygiene.webhookUrl` | | `HYGIENE_WEBHOOK_URL` for hygiene notifications | `""` |
| `hygiene.existingSecret` | | Secret holding the webhook URL | `""` |
| `hygiene.existingSecretWebhookUrlKey` | | Key inside that secret | `hygiene-webhook-url` |
| `attention.pendingAgeSlo` | `--attention-pending-age-slo` | Pending-age SLO (binary default `5m`) | `""` |
| `attention.retryStormThreshold` | `--attention-retry-storm-threshold` | Retry-storm threshold (binary default `1000`) | `""` |
| `attention.pausedLongAfter` | `--attention-paused-long-after` | "Paused too long" age (binary default `168h`) | `""` |
| `attention.groupStallAfter` | `--attention-group-stall-after` | "Group stalled" age (binary default `5m`) | `""` |

See `values.yaml` for the full list.

## Testing the chart

`./scripts/helm-render-check.sh` lints the chart, renders it with several
value combinations, checks that the combinations the binary rejects fail at
render time, and compares the flags the chart emits with the flags this README
documents. CI runs the same script.
