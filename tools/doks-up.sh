#!/usr/bin/env bash
# Provision the demonstration cluster on DigitalOcean Kubernetes.
#
# The platform itself is portable. Every module is plain Kubernetes plus Helm, so this
# script only has to reproduce the two properties the kind cluster provides that a stock
# managed cluster does not:
#
#   1. a tainted platform node pool, so M5 can scale application workloads without
#      contending with the components measuring them;
#   2. a CNI that genuinely enforces NetworkPolicy, without which M6 is decoration.
#
# It deliberately stops before installing anything else. `make gitops-bootstrap` is the
# same command here as on kind, and that is the point: if the platform needed a different
# bootstrap per environment, it would not really be GitOps.
set -euo pipefail

CLUSTER=${CLUSTER:-gitops-platform}
REGION=${REGION:-fra1}
# Sized from measurements on the kind cluster, not from guesswork. The platform's steady
# state is roughly 2.5 cores; the load test briefly needs headroom for six worker replicas
# on top. Two nodes was tried on the laptop equivalent and the API server began timing out
# under reconciliation load, hence three, with the control plane managed and therefore
# not competing with any of it.
SIZE=${SIZE:-s-2vcpu-4gb}
APP_NODES=${APP_NODES:-2}

command -v doctl >/dev/null 2>&1 || {
  echo "doctl is not installed. brew install doctl, then: doctl auth init"
  exit 1
}
doctl account get >/dev/null 2>&1 || {
  echo "doctl is not authenticated. Run: doctl auth init"
  exit 1
}

if doctl kubernetes cluster get "$CLUSTER" >/dev/null 2>&1; then
  echo "cluster '$CLUSTER' already exists; reusing it"
else
  echo "==> creating cluster '$CLUSTER' in $REGION"
  echo "    1 platform node (tainted) + ${APP_NODES} application nodes, ${SIZE}"
  # The taint is what keeps M5's replicas off the node running Prometheus, ArgoCD and the
  # cost exporter. Without it a scale-up would evict or starve the very components
  # measuring the scale-up.
  doctl kubernetes cluster create "$CLUSTER" \
    --region "$REGION" \
    --auto-upgrade=false \
    --surge-upgrade=false \
    --node-pool "name=platform;size=${SIZE};count=1;label=workload=platform;taint=workload=platform:NoSchedule" \
    --node-pool "name=apps;size=${SIZE};count=${APP_NODES};label=workload=apps" \
    --wait
fi

echo "==> writing kubeconfig and switching context"
doctl kubernetes cluster kubeconfig save "$CLUSTER" >/dev/null
kubectl config current-context

echo "==> waiting for nodes"
kubectl wait --for=condition=Ready node --all --timeout=600s >/dev/null
kubectl get nodes -L workload

echo
echo "==> verifying the CNI actually enforces NetworkPolicy"
# DOKS ships Cilium, which does enforce policy, but "the vendor says so" is exactly the
# assumption that makes M6 worthless when it turns out to be wrong. The same test that
# gates the kind cluster gates this one, and it is the reason no CNI is installed here:
# if this passes, adding Calico would be replacing a working dataplane for no reason.
if bash "$(dirname "$0")/verify-cni.sh"; then
  echo
  echo "The bundled CNI enforces NetworkPolicy. No CNI replacement needed."
else
  echo
  echo "The bundled CNI does NOT enforce NetworkPolicy. M6 cannot be trusted on this"
  echo "cluster. Install Calico before continuing:  make cni"
  exit 1
fi

cat <<EOF

Cluster is ready. Next:

  make gitops-bootstrap     # identical to kind. ArgoCD, repo access, root Application
  make doks-forward         # local ports for Grafana, ArgoCD and the demo API
  make verify               # run every proof the platform makes about itself

Tear down when the demonstration is over. This bills by the hour:

  make doks-down
EOF
