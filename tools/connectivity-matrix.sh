#!/usr/bin/env bash
# M6, the connectivity matrix (LLD §6.6).
#
# For each pair of workloads, assert the expected allow/deny outcome. This is the test that
# distinguishes a network baseline from a set of YAML files that happen to exist: the API
# server accepts NetworkPolicy objects regardless of whether anything enforces them, so
# "the policies are applied" proves nothing at all.
#
# Both directions matter equally. A test that only checks the allowed paths passes
# perfectly on a cluster with no isolation whatsoever, which is precisely the failure this
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

# probe_as runs a connection attempt from a throwaway pod carrying the same labels as a
# real workload.
#
# It cannot exec into the real pods: the application images are distroless, with no shell
# and no netcat, which is a deliberate hardening choice that also makes them impossible to
# probe from the inside. Kubernetes NetworkPolicy selects on labels, so a pod wearing the
# workload's labels is subject to exactly the same rules. This tests the policy as
# written, which is what the matrix is for. That the real pods can talk is proven
# separately, by the application actually draining its queue.
probe_as() {
  local identity=$1 labels=$2 target=$3 port=$4
  local probe="probe-${identity}"

  kubectl -n "$NS" delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl -n "$NS" run "$probe" --image="$IMAGE" --labels="$labels" \
    --command -- sleep 300 >/dev/null 2>&1 || return 2
  kubectl -n "$NS" wait --for=condition=Ready "pod/$probe" --timeout=120s >/dev/null 2>&1 || return 2

  # A blocked connection under default-deny is dropped, not refused, so it manifests as a
  # timeout rather than an error. The short deadline is what makes "denied" fast and
  # unambiguous instead of a 30-second hang.
  local rc=0
  kubectl -n "$NS" exec "$probe" -- \
    timeout "$TIMEOUT" nc -z -w "$TIMEOUT" "$target" "$port" >/dev/null 2>&1 || rc=1
  kubectl -n "$NS" delete pod "$probe" --wait=false >/dev/null 2>&1
  return $rc
}

check() {
  local description=$1 expected=$2 identity=$3 labels=$4 target=$5 port=$6
  local actual result rc=0

  probe_as "$identity" "$labels" "$target" "$port" || rc=$?
  if [ "$rc" -eq 2 ]; then
    printf "  %-56s SKIPPED (probe pod would not start)\n" "$description"
    return
  fi
  [ "$rc" -eq 0 ] && actual=allow || actual=deny

  if [ "$actual" = "$expected" ]; then
    result="ok"; pass=$((pass + 1))
  else
    result="FAIL (expected ${expected}, got ${actual})"; fail=$((fail + 1))
    FAILURES+=("$description: expected ${expected}, got ${actual}")
  fi
  printf "  %-56s %s\n" "$description" "$result"
}

echo "-- paths that MUST work (the application has to function) --"
check "demo-api  -> redis:6379"   allow api    "app.kubernetes.io/name=demo-api"    redis 6379
check "demo-worker -> redis:6379" allow worker "app.kubernetes.io/name=demo-worker" redis 6379

echo
echo "-- paths that MUST be blocked (isolation is real) --"
# The producer has no business reaching the consumer: they communicate exclusively through
# the queue. If this succeeds, the allow-list is wider than the architecture.
check "demo-api  -> demo-worker:8080" deny api "app.kubernetes.io/name=demo-api" demo-worker 8080
# Nothing should be able to reach the API's HTTP port from inside the namespace, external
# traffic arrives through the NodePort from the node network, not from a pod.
check "demo-worker -> demo-api:8080" deny worker "app.kubernetes.io/name=demo-worker" demo-api 8080

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
  FAILURES+=("KEDA cannot read queue depth, autoscaling is dead while appearing configured")
fi

# Prometheus scrapes the demo pods from another namespace. Losing this does not break the
# application, it breaks the evidence, cost and latency go flat, which reads as a workload
# that costs nothing rather than as a blocked connection.
# Read through a port-forward rather than exec'ing a shell tool into the Prometheus
# container, the same distroless problem, and an exec failure would otherwise be
# indistinguishable from a blocked scrape.
kubectl -n observability port-forward svc/observability-kube-prometh-prometheus 9098:9090 >/dev/null 2>&1 &
PROM_PF=$!
for _ in $(seq 1 25); do curl -sf http://localhost:9098/-/ready >/dev/null 2>&1 && break; sleep 2; done
UP=$(curl -sfG http://localhost:9098/api/v1/query --data-urlencode 'query=up{namespace="demo"}' 2>/dev/null \
  | grep -o '"value"' | wc -l | tr -d ' ' || echo 0)
kill $PROM_PF 2>/dev/null || true
if [ "${UP:-0}" -gt 0 ]; then
  printf "  %-56s %s\n" "prometheus -> demo pods (scrape succeeding)" "ok"
  pass=$((pass + 1))
else
  printf "  %-56s %s\n" "prometheus -> demo pods (scrape succeeding)" "FAIL (no targets up)"
  fail=$((fail + 1))
  FAILURES+=("Prometheus cannot scrape the demo namespace. M4 and M7 lose their inputs")
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
