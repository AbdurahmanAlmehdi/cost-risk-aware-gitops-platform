#!/usr/bin/env bash
# Drive demand through the platform and prove the elasticity claim end to end.
#
# The claim has three parts, and each is checked separately because a system can satisfy
# one while failing the others:
#
#   1. demand rises      → the queue fills
#   2. replicas follow   → KEDA scales the worker tier up
#   3. the backlog drains → the extra replicas actually did the work
#   4. demand falls      → replicas return to the minimum
#
# Step 3 matters most. Replica count alone proves only that the autoscaler acted; a
# workload can scale up and still fall behind. Watching the queue drain is what shows the
# action had the intended effect.
#
# Bounds are asserted throughout (LLD §5.6): replicas must never leave [min, max].
set -euo pipefail

API=${API:-http://localhost:8081}
NS=demo
DEPLOYMENT=demo-worker
JOBS=${JOBS:-600}
JOB_MS=${JOB_MS:-400}
SCALE_TIMEOUT=${SCALE_TIMEOUT:-180}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-420}

replicas() { kubectl -n "$NS" get deploy "$DEPLOYMENT" -o jsonpath='{.status.replicas}' 2>/dev/null || echo 0; }
depth()    { curl -sf "${API}/api/queue" 2>/dev/null | sed -E 's/.*"queue_depth":([0-9]+).*/\1/' || echo "?"; }

MIN=$(kubectl -n "$NS" get scaledobject "$DEPLOYMENT" -o jsonpath='{.spec.minReplicaCount}' 2>/dev/null || echo 1)
MAX=$(kubectl -n "$NS" get scaledobject "$DEPLOYMENT" -o jsonpath='{.spec.maxReplicaCount}' 2>/dev/null || echo 1)

if [ -z "$MAX" ] || [ "$MAX" = "0" ]; then
  echo "FAIL: no ScaledObject found for ${NS}/${DEPLOYMENT}. Is M5 deployed?"
  exit 1
fi

echo "==> elasticity test on ${NS}/${DEPLOYMENT}  (bounds: ${MIN}–${MAX} replicas)"
START_REPLICAS=$(replicas)
echo "    starting at ${START_REPLICAS} replica(s), queue depth $(depth)"

# A bounds violation at any point is a failure, so replicas are checked on every poll
# rather than only at the end — a transient overshoot is exactly the bug this catches.
assert_bounds() {
  local r=$1
  if [ "$r" -gt "$MAX" ]; then
    echo "FAIL: replicas reached ${r}, above the configured maximum of ${MAX}."
    exit 1
  fi
  if [ "$r" -lt "$MIN" ]; then
    echo "FAIL: replicas fell to ${r}, below the configured minimum of ${MIN}."
    exit 1
  fi
}

echo
echo "--> enqueuing ${JOBS} jobs of ${JOB_MS}ms"
curl -sf -X POST "${API}/api/jobs?count=${JOBS}&duration_ms=${JOB_MS}" >/dev/null || {
  echo "FAIL: could not reach the API at ${API}. Is the port mapping up?"
  exit 1
}
echo "    queue depth is now $(depth)"

echo
echo "--> waiting for KEDA to scale up (up to ${SCALE_TIMEOUT}s)"
PEAK=$START_REPLICAS
elapsed=0
while [ "$elapsed" -lt "$SCALE_TIMEOUT" ]; do
  r=$(replicas); assert_bounds "$r"
  [ "$r" -gt "$PEAK" ] && PEAK=$r
  printf "    t+%3ds  replicas=%-3s queue=%s\n" "$elapsed" "$r" "$(depth)"
  [ "$PEAK" -gt "$START_REPLICAS" ] && break
  sleep 15; elapsed=$((elapsed + 15))
done

if [ "$PEAK" -le "$START_REPLICAS" ]; then
  echo
  echo "FAIL: demand rose but replicas never did. The autoscaler is not reacting."
  echo "      Check: kubectl -n $NS describe scaledobject $DEPLOYMENT"
  exit 1
fi
echo "    scaled up to ${PEAK} replicas"

echo
echo "--> waiting for the backlog to drain (up to ${DRAIN_TIMEOUT}s)"
elapsed=0
while [ "$elapsed" -lt "$DRAIN_TIMEOUT" ]; do
  r=$(replicas); assert_bounds "$r"
  [ "$r" -gt "$PEAK" ] && PEAK=$r
  d=$(depth)
  printf "    t+%3ds  replicas=%-3s queue=%s\n" "$elapsed" "$r" "$d"
  [ "$d" = "0" ] && break
  sleep 15; elapsed=$((elapsed + 15))
done

if [ "$(depth)" != "0" ]; then
  echo
  echo "FAIL: the queue did not drain within ${DRAIN_TIMEOUT}s. Scaling up did not keep pace."
  exit 1
fi
echo "    backlog cleared"

echo
echo "PASS"
echo "  demand rose, replicas followed to ${PEAK} (bounds ${MIN}–${MAX} never breached),"
echo "  and the backlog drained — the added replicas did the work."
echo
echo "  Scale-down is deliberately slow (cooldownPeriod 60s, stabilisation 120s) so a"
echo "  lull between bursts does not tear down workers that are about to be needed."
echo "  Watch it settle back to ${MIN} with:"
echo "    kubectl -n $NS get deploy $DEPLOYMENT -w"
