#!/usr/bin/env bash
# Render the asynqmon chart with several value combinations and check the
# result. A chart that lints can still render a Deployment that crash-loops,
# so this script asserts on the rendered manifests themselves.
#
# Usage: ./scripts/helm-render-check.sh
set -euo pipefail

cd "$(dirname "$0")/.."
CHART=charts/asynqmon
FULL_VALUES="${CHART}/ci/full-values.yaml"
failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

pass() { echo "ok: $*"; }

# render <name> <helm args...>
render() {
  local name=$1
  shift
  if ! helm template rel "${CHART}" "$@" >/tmp/asynqmon-render.yaml 2>/tmp/asynqmon-render.err; then
    fail "${name}: helm template failed"
    sed 's/^/    /' /tmp/asynqmon-render.err >&2
    return 1
  fi
  pass "renders: ${name}"
}

# render_must_fail <name> <expected substring> <helm args...>
render_must_fail() {
  local name=$1 expect=$2
  shift 2
  if helm template rel "${CHART}" "$@" >/dev/null 2>/tmp/asynqmon-render.err; then
    fail "${name}: expected the render to fail, it succeeded"
    return
  fi
  if ! grep -q -- "${expect}" /tmp/asynqmon-render.err; then
    fail "${name}: error message does not mention '${expect}'"
    sed 's/^/    /' /tmp/asynqmon-render.err >&2
    return
  fi
  pass "rejected: ${name}"
}

contains() {
  local what=$1
  grep -q -- "${what}" /tmp/asynqmon-render.yaml || fail "expected the render to contain '${what}'"
}

lacks() {
  local what=$1
  ! grep -q -- "${what}" /tmp/asynqmon-render.yaml || fail "expected the render NOT to contain '${what}'"
}

echo "== lint"
helm lint --strict "${CHART}"
helm lint --strict "${CHART}" -f "${FULL_VALUES}"

echo
echo "== value combinations"

render "defaults"
contains "automountServiceAccountToken: false"
contains "type: RuntimeDefault"
contains "timeoutSeconds: 4"
contains "failureThreshold: 2"
contains "GOMEMLIMIT"
contains "cpu: 50m"

render "redis url" --set redis.url=redis://redis.example:6379/0
contains "--redis-url=\$(REDIS_URL)"

render "redis addr with an existing password secret" \
  --set redis.addr=redis.example:6379 \
  --set redis.existingSecret=redis-creds \
  --set redis.existingSecretPasswordKey=password
contains "name: REDIS_PASSWORD"
contains "name: redis-creds"

render "redis cluster with TLS" \
  --set redis.clusterNodes=n1:6379\\,n2:6379 \
  --set redis.tls=redis.example
contains "--redis-cluster-nodes=\$(REDIS_CLUSTER_NODES)"

# The whole URL comes from a secret, so the password never appears in the
# Deployment spec, and REDIS_PASSWORD is not emitted (the binary ignores it
# in URL mode).
render "redis url from a secret" \
  --set redis.existingSecret=redis-creds \
  --set redis.existingSecretUrlKey=url
contains "key: url"
lacks "name: REDIS_PASSWORD"

render "three replicas with a PDB and spread constraints" \
  -f "${FULL_VALUES}"
contains "kind: PodDisruptionBudget"
contains "minAvailable: 1"
contains "topologySpreadConstraints"
contains "kind: ServiceMonitor"
contains "kind: Ingress"
# Inline secrets land in a Secret object, not in the pod spec.
lacks "value: \"prom:pass\""
lacks "value: \"s3cret\""

render "maxUnavailable form of the PDB" \
  --set replicaCount=3 --set pdb.enabled=true \
  --set pdb.minAvailable=null --set pdb.maxUnavailable=1
contains "maxUnavailable: 1"

echo
echo "== rejected combinations"
render_must_fail "redis.url with redis.password" "mutually exclusive" \
  --set redis.url=redis://redis.example:6379/0 --set redis.password=s3cret
render_must_fail "redis.url with redis.existingSecret" "mutually exclusive" \
  --set redis.url=redis://redis.example:6379/0 --set redis.existingSecret=redis-creds
render_must_fail "existingSecretUrlKey without existingSecret" "needs redis.existingSecret" \
  --set redis.existingSecretUrlKey=url
render_must_fail "serviceMonitor without the exporter" "enableExporter" \
  --set serviceMonitor.enabled=true

echo
echo "== flag parity between the chart and its README"
# Every flag the chart can emit must be documented, and every documented flag
# must be emittable. The check runs against the README table, not against the
# binary: the chart and the docs are what an operator reads.
# The three redis modes are mutually exclusive, so collect the union of the
# flags each mode renders.
{
  helm template rel "${CHART}" -f "${FULL_VALUES}"
  helm template rel "${CHART}" -f "${FULL_VALUES}" \
    --set redis.addr="" --set redis.password="" \
    --set redis.url=redis://redis.example:6379/0
  helm template rel "${CHART}" -f "${FULL_VALUES}" \
    --set redis.addr="" --set redis.clusterNodes=n1:6379\\,n2:6379
} >/tmp/asynqmon-render.yaml
rendered=$(grep -oE '^[[:space:]]+- "--[a-z-]+' /tmp/asynqmon-render.yaml | grep -oE -- '--[a-z-]+' | sort -u || true)
documented=$(grep -oE -- '`--[a-z-]+' "${CHART}/README.md" | grep -oE -- '--[a-z-]+' | sort -u || true)

undocumented=$(comm -23 <(echo "${rendered}") <(echo "${documented}") || true)
unrenderable=$(comm -13 <(echo "${rendered}") <(echo "${documented}") || true)

if [ -n "${undocumented}" ]; then
  fail "the chart emits flags the chart README does not document:"
  echo "${undocumented}" | sed 's/^/    /' >&2
else
  pass "every rendered flag is documented"
fi
if [ -n "${unrenderable}" ]; then
  fail "the chart README documents flags no value can render:"
  echo "${unrenderable}" | sed 's/^/    /' >&2
else
  pass "every documented flag has a values key"
fi

echo
if [ "${failures}" -ne 0 ]; then
  echo "${failures} check(s) failed" >&2
  exit 1
fi
echo "all checks passed"
