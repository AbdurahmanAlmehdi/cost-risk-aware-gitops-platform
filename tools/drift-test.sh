#!/usr/bin/env bash
# Prove that Git is the authority on cluster state, not merely its usual source.
#
# The test edits a live resource directly — the way an engineer would during an incident —
# and asserts the platform reverts it without anyone intervening. Until that is
# demonstrated, "GitOps" describes how the cluster was populated, not how it is governed:
# a cluster that accepts manual edits has two sources of truth and no way to tell which
# one is currently winning.
#
# The field under test is deliberately NOT replicas. The platform excludes
# .spec.replicas from diffing so that KEDA can scale workers without ArgoCD reverting it
# (see platform/argocd/values.yaml), so replicas would prove the opposite of the point.
set -euo pipefail

NS=demo
DEPLOYMENT=demo-api
CONTAINER=api
DRIFTED_VALUE=999Mi
TIMEOUT=180

current_limit() {
  kubectl -n "$NS" get deploy "$DEPLOYMENT" \
    -o jsonpath="{.spec.template.spec.containers[?(@.name=='$CONTAINER')].resources.limits.memory}"
}

echo "==> drift test on ${NS}/${DEPLOYMENT}"

DECLARED=$(current_limit)
if [ -z "$DECLARED" ]; then
  echo "FAIL: could not read the current memory limit. Is the workload deployed?"
  exit 1
fi
echo "--> Git declares memory limit: $DECLARED"

if [ "$DECLARED" = "$DRIFTED_VALUE" ]; then
  echo "FAIL: the declared value already equals the drift value; the test would prove nothing."
  exit 1
fi

echo "--> introducing drift: setting the limit to $DRIFTED_VALUE directly on the cluster"

# Confirm the drift landed by reading what the API server returned in the patch response,
# NOT by reading the object back afterwards.
#
# A re-read is a race against the very thing being tested. Self-heal here reverts in about
# a second, so the follow-up GET returns the restored value and the test concludes the patch
# never applied — reporting FAIL on a platform that is working perfectly, and faster than
# expected at that. It went unnoticed while the only cluster was a laptop kind cluster,
# where reversion took ~5s and the race was comfortably won.
#
# The patch response is the authoritative record that the write was accepted and applied.
APPLIED=$(kubectl -n "$NS" patch deploy "$DEPLOYMENT" --type=json -p "$(cat <<EOF
[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"$DRIFTED_VALUE"}]
EOF
)" -o jsonpath="{.spec.template.spec.containers[?(@.name=='$CONTAINER')].resources.limits.memory}")

if [ "$APPLIED" != "$DRIFTED_VALUE" ]; then
  echo "FAIL: the drift patch did not apply, so there is nothing to detect."
  echo "      The API server returned '$APPLIED' rather than '$DRIFTED_VALUE'."
  exit 1
fi
echo "    drift applied — the cluster now disagrees with Git"

echo "--> waiting for self-heal to revert it (up to ${TIMEOUT}s)"
elapsed=0
while [ "$elapsed" -lt "$TIMEOUT" ]; do
  if [ "$(current_limit)" = "$DECLARED" ]; then
    echo
    if [ "$elapsed" -eq 0 ]; then
      echo "PASS: the platform had already reverted the manual change before the first check."
      echo "      Self-heal is faster than this test can observe, which is the good direction"
      echo "      to fail to measure something in."
    else
      echo "PASS: the platform reverted the manual change after ~${elapsed}s."
    fi
    echo "      Live state was corrected to match Git with no human involvement."
    exit 0
  fi
  sleep 5
  elapsed=$((elapsed + 5))
done

echo
echo "FAIL: the manual change survived ${TIMEOUT}s. Self-heal is not reverting drift."
echo "      Check: kubectl -n argocd get application $DEPLOYMENT -o jsonpath='{.status.sync}'"
exit 1
