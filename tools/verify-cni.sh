#!/usr/bin/env bash
# Assert that the installed CNI *enforces* NetworkPolicy rather than merely accepting it.
#
# This distinction matters enough to test at bootstrap: the Kubernetes API server will
# happily store a NetworkPolicy object under a CNI that ignores it, so `kubectl get
# networkpolicy` proves nothing. M6's entire security claim rests on enforcement, so the
# platform verifies enforcement before anything is built on top of it.
#
# Method: two pods, one policy, two probes — the denied path must fail and the allowed
# path must succeed. A CNI that ignores policy passes the second probe and fails the
# first, which is exactly what we catch.
set -euo pipefail

NS=cni-verify
IMAGE=registry.k8s.io/e2e-test-images/agnhost:2.53

cleanup() { kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> verifying NetworkPolicy enforcement in namespace '$NS'"
kubectl delete namespace "$NS" --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl create namespace "$NS" >/dev/null

kubectl -n "$NS" run server --image="$IMAGE" --labels="app=server" \
  --port=8080 --command -- /agnhost netexec --http-port=8080 >/dev/null
kubectl -n "$NS" expose pod server --port=8080 >/dev/null
kubectl -n "$NS" run client --image="$IMAGE" --labels="app=client" \
  --command -- sleep 3600 >/dev/null

kubectl -n "$NS" wait --for=condition=Ready pod/server pod/client --timeout=120s >/dev/null

probe() {
  # Returns 0 if the client can reach the server, 1 otherwise. The short timeout is what
  # turns a silent drop (default-deny) into a fast, deterministic failure.
  kubectl -n "$NS" exec client -- \
    curl -sS --max-time 5 -o /dev/null "http://server:8080/hostname" >/dev/null 2>&1
}

echo "--> baseline: no policy, connection should SUCCEED"
if ! probe; then
  echo "FAIL: pods cannot reach each other before any policy is applied."
  echo "      This is a cluster/CNI health problem, not a policy problem."
  exit 1
fi
echo "    ok"

echo "--> applying default-deny ingress, connection should now FAIL"
kubectl -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
spec:
  podSelector: {}
  policyTypes: ["Ingress"]
EOF

# Policy programming is asynchronous; poll rather than sleeping a fixed guess.
for i in $(seq 1 15); do
  probe || break
  [ "$i" -eq 15 ] && {
    echo "FAIL: traffic still flows with default-deny in force."
    echo "      The CNI is not enforcing NetworkPolicy. M6 cannot be trusted on this cluster."
    exit 1
  }
  sleep 2
done
echo "    ok — denied"

echo "--> applying an explicit allow for app=client, connection should SUCCEED again"
kubectl -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-client-to-server
spec:
  podSelector:
    matchLabels:
      app: server
  policyTypes: ["Ingress"]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: client
      ports:
        - protocol: TCP
          port: 8080
EOF

for i in $(seq 1 15); do
  probe && break
  [ "$i" -eq 15 ] && {
    echo "FAIL: the explicit allow-list rule did not restore connectivity."
    exit 1
  }
  sleep 2
done
echo "    ok — allowed"

echo
echo "PASS: the CNI enforces NetworkPolicy in both directions (deny and allow)."
