#!/usr/bin/env bash
# Expose the platform's three surfaces locally.
#
# The kind cluster maps these through the host automatically (platform/kind/cluster.yaml);
# a managed cluster has no such mapping, so the same ports are forwarded instead. Keeping
# the port numbers identical means every other tool, the load test, the reconciliation
# check, the demo script, works unchanged on both.
#
# Port-forwarding rather than LoadBalancers is deliberate for a demonstration cluster:
# three LoadBalancers cost more per month than the nodes, and nothing here needs to be
# reachable by anyone but the presenter. For a URL a supervisor can visit unattended, add
# a Cloudflare Tunnel, see docs/DEPLOY-DOKS.md.
set -euo pipefail

declare -a PIDS=()
cleanup() {
  echo
  echo "closing port-forwards"
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT INT TERM

forward() {
  local ns=$1 target=$2 ports=$3 label=$4
  kubectl -n "$ns" port-forward "$target" "$ports" >/dev/null 2>&1 &
  PIDS+=($!)
  printf "  %-14s http://localhost:%s\n" "$label" "${ports%%:*}"
}

echo "forwarding platform surfaces:"
forward argocd        svc/argocd-server                             8080:80   "ArgoCD"
forward observability svc/observability-grafana                     3000:80   "Grafana"
forward demo          svc/demo-api                                  8081:8080 "Demo API"

echo
echo "  ArgoCD password:  make argocd-password"
echo "  Grafana password: make grafana-password"
echo
echo "Leave this running. Ctrl-C closes all three."
wait
