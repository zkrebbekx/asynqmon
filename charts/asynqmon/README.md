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

Cloud IAM auth (AWS IRSA) — annotate the ServiceAccount so the pod assumes the
role, then enable IAM on the binary via `extraArgs`:

```bash
helm install asynqmon ./charts/asynqmon \
  --set redis.clusterNodes=clustercfg.my-cache.use1.cache.amazonaws.com:6379 \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::123456789012:role/asynqmon \
  --set-string 'extraArgs[0]=--redis-iam-auth' \
  --set-string 'extraArgs[1]=--redis-iam-provider=aws' \
  --set-string 'extraArgs[2]=--aws-region=us-east-1' \
  --set-string 'extraArgs[3]=--redis-iam-cache-name=my-cache' \
  --set-string 'extraArgs[4]=--redis-user=my-iam-user'
```

## Key values

| Key | Description | Default |
| --- | --- | --- |
| `image.repository` | Image repo | `ghcr.io/zkrebbekx/asynqmon` |
| `image.tag` | Image tag | chart `appVersion` |
| `redis.url` / `redis.addr` / `redis.clusterNodes` | Redis connection (pick one) | `""` |
| `redis.existingSecret` | Secret holding the redis password | `""` |
| `readOnly` | Run UI in read-only mode | `false` |
| `prometheus.enableExporter` | Expose `/metrics` | `false` |
| `prometheus.address` | Prometheus server for the metrics view | `""` |
| `serviceAccount.annotations` | IRSA / Workload Identity annotations | `{}` |
| `ingress.enabled` | Create an Ingress | `false` |
| `serviceMonitor.enabled` | Prometheus-Operator ServiceMonitor (needs exporter) | `false` |
| `extraArgs` | Extra binary flags | `[]` |

See `values.yaml` for the full list.
