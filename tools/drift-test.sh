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
kubectl -n "$NS" patch deploy "$DEPLOYMENT" --type=json -p "$(cat <<EOF
[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"$DRIFTED_VALUE"}]
EOF
)" >/dev/null

# Confirm the drift actually landed. Without this the test could "pass" simply because
# the patch silently failed and the value never changed.
if [ "$(current_limit)" != "$DRIFTED_VALUE" ]; then
  echo "FAIL: the drift patch did not apply, so there is nothing to detect."
  exit 1
fi
echo "    drift applied — the cluster now disagrees with Git"

echo "--> waiting for self-heal to revert it (up to ${TIMEOUT}s)"
elapsed=0
while [ "$elapsed" -lt "$TIMEOUT" ]; do
  if [ "$(current_limit)" = "$DECLARED" ]; then
    echo
    echo "PASS: the platform reverted the manual change after ~${elapsed}s."
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
