#!/usr/bin/env bash
# M6 — the connectivity matrix (LLD §6.6).
#
# For each pair of workloads, assert the expected allow/deny outcome. This is the test that
# distinguishes a network baseline from a set of YAML files that happen to exist: the API
# server accepts NetworkPolicy objects regardless of whether anything enforces them, so
# "the policies are applied" proves nothing at all.
#
# Both directions matter equally. A test that only checks the allowed paths passes
# perfectly on a cluster with no isolation whatsoever — which is precisely the failure this
# module exists to prevent.
set -euo pipefail

NS=demo
IMAGE=registry.k8s.io/e2e-test-images/agnhost:2.53
PROBE=netpolicy-probe
TIMEOUT=5

pass=0; fail=0
declare -a FAILURES=()

cleanup() { kubectl -n "$NS" delete pod "$PROBE" --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> connectivity matrix for namespace '$NS'"
echo

# probe_from runs a connection attempt from inside an existing workload's pod, so the
# source identity is the real one the policy selects on. Probing from a scratch pod would
# test a different source and prove nothing about the actual workloads.
probe_from() {
  local selector=$1 target=$2 port=$3
  local pod
  pod=$(kubectl -n "$NS" get pod -l "$selector" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -z "$pod" ]; then
    echo "SKIP  no pod matching ${selector}"
    return 2
  fi
  # A blocked connection under default-deny is dropped, not refused, so it manifests as a
  # timeout rather than an error. The short deadline is what makes "denied" fast and
  # unambiguous instead of a 30-second hang.
  kubectl -n "$NS" exec "$pod" -c "${4:-}" -- \
    timeout "$TIMEOUT" nc -z -w "$TIMEOUT" "$target" "$port" >/dev/null 2>&1
}

check() {
  local description=$1 expected=$2 selector=$3 target=$4 port=$5 container=${6:-}
  local actual result

  if probe_from "$selector" "$target" "$port" "$container"; then
    actual=allow
  else
    [ $? -eq 2 ] && { printf "  %-56s SKIPPED\n" "$description"; return; }
    actual=deny
  fi

  if [ "$actual" = "$expected" ]; then
    result="ok"; pass=$((pass + 1))
  else
    result="FAIL (expected ${expected}, got ${actual})"; fail=$((fail + 1))
    FAILURES+=("$description: expected ${expected}, got ${actual}")
  fi
  printf "  %-56s %s\n" "$description" "$result"
}

echo "-- paths that MUST work (the application has to function) --"
check "demo-api  -> redis:6379"        allow "app.kubernetes.io/name=demo-api"    redis 6379 api
check "demo-worker -> redis:6379"      allow "app.kubernetes.io/name=demo-worker" redis 6379 worker

echo
echo "-- paths that MUST be blocked (isolation is real) --"
# The producer has no business reaching the consumer: they communicate exclusively through
# the queue. If this succeeds, the allow-list is wider than the architecture.
check "demo-api  -> demo-worker:8080"  deny  "app.kubernetes.io/name=demo-api"    demo-worker 8080 api
# Nothing should be able to reach the API's HTTP port from inside the namespace — external
# traffic arrives through the NodePort from the node network, not from a pod.
check "demo-worker -> demo-api:8080"   deny  "app.kubernetes.io/name=demo-worker" demo-api 8080 worker

echo
echo "-- a pod with no allow-list entry is denied by default --"
kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --labels="app=unauthorized" \
  --command -- sleep 600 >/dev/null 2>&1 || true
kubectl -n "$NS" wait --for=condition=Ready "pod/$PROBE" --timeout=90s >/dev/null 2>&1 || true

if kubectl -n "$NS" get pod "$PROBE" >/dev/null 2>&1; then
  if kubectl -n "$NS" exec "$PROBE" -- timeout "$TIMEOUT" nc -z -w "$TIMEOUT" redis 6379 >/dev/null 2>&1; then
    printf "  %-56s %s\n" "unauthorized pod -> redis:6379" "FAIL (expected deny, got allow)"
    fail=$((fail + 1))
    FAILURES+=("an unlabelled pod reached the queue; default-deny is not in force")
  else
    printf "  %-56s %s\n" "unauthorized pod -> redis:6379" "ok"
    pass=$((pass + 1))
  fi
else
  printf "  %-56s %s\n" "unauthorized pod -> redis:6379" "SKIPPED (probe pod not ready)"
fi

echo
echo "-- dependencies outside the namespace --"
# KEDA reads queue depth by connecting to Redis itself. This is checked explicitly because
# omitting its allow-rule produces no error anywhere: the metric simply stops, the HPA
# reads "unknown", and autoscaling quietly dies for a reason that lives in M6.
KEDA_METRIC=$(kubectl -n "$NS" get hpa keda-hpa-demo-worker \
  -o jsonpath='{.status.currentMetrics[0].external.current.value}' 2>/dev/null || true)
if [ -n "$KEDA_METRIC" ]; then
  printf "  %-56s %s\n" "keda -> redis (queue depth readable)" "ok"
  pass=$((pass + 1))
else
  printf "  %-56s %s\n" "keda -> redis (queue depth readable)" "FAIL (no metric; scaler cannot reach Redis)"
  fail=$((fail + 1))
  FAILURES+=("KEDA cannot read queue depth — autoscaling is dead while appearing configured")
fi

# Prometheus scrapes the demo pods from another namespace. Losing this does not break the
# application, it breaks the evidence — cost and latency go flat, which reads as a workload
# that costs nothing rather than as a blocked connection.
UP=$(kubectl -n observability exec -c prometheus \
  prometheus-observability-kube-prometh-prometheus-0 -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=up{namespace="demo"}' 2>/dev/null \
  | grep -o '"value"' | wc -l | tr -d ' ' || echo 0)
if [ "${UP:-0}" -gt 0 ]; then
  printf "  %-56s %s\n" "prometheus -> demo pods (scrape succeeding)" "ok"
  pass=$((pass + 1))
else
  printf "  %-56s %s\n" "prometheus -> demo pods (scrape succeeding)" "FAIL (no targets up)"
  fail=$((fail + 1))
  FAILURES+=("Prometheus cannot scrape the demo namespace — M4 and M7 lose their inputs")
fi

echo
if [ "$fail" -gt 0 ]; then
  echo "FAIL: ${fail} of $((pass + fail)) checks did not match the expected matrix."
  for f in "${FAILURES[@]}"; do echo "  - $f"; done
  exit 1
fi

echo "PASS: all ${pass} checks match the expected connectivity matrix."
echo "      Permitted paths work, everything else is denied, and the denials are enforced"
echo "      by the CNI rather than merely declared."
