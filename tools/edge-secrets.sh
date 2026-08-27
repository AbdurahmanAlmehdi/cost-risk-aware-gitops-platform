#!/usr/bin/env bash
# Install the two credentials the edge stack needs, straight into the cluster.
#
# Neither is committed. The Caddyfile in Git references them by environment variable, so
# the routing configuration is reviewable without the password being in the repository.
#
# A production estate would keep these in Git too, encrypted with SOPS or sealed-secrets,
# so the whole system could be rebuilt from the repository alone. That is deliberately out
# of scope here: it needs a key-management story, and this cluster is disposable.
set -euo pipefail

NS=edge
TUNNEL_NAME=${TUNNEL_NAME:-gitops-platform}

kubectl get namespace "$NS" >/dev/null 2>&1 || kubectl create namespace "$NS" >/dev/null

# The shared basic-auth password this script used to generate is gone: reviewers now
# authenticate individually through Cloudflare Access, so there is no second credential to
# distribute. See the header of manifests/apps/edge/caddyfile.yaml.
#
# That also removes this script's dependency on a running Docker daemon, which was needed
# only to run `caddy hash-password`. It now needs nothing but kubectl and cloudflared.

# --- tunnel token ------------------------------------------------------------
TOKEN=$(cloudflared tunnel token "$TUNNEL_NAME" 2>/dev/null | tail -1)
if [ -z "$TOKEN" ]; then
  echo "could not read the tunnel token. Is '$TUNNEL_NAME' created and cloudflared logged in?"
  exit 1
fi
kubectl -n "$NS" create secret generic cloudflared-token \
  --from-literal=token="$TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
echo "tunnel token installed"

echo
echo "  Reviewers are authorised by email in Cloudflare Access, not by a shared password."
echo "  Add one to the 'gitops-platform reviewers' Access group and they can sign in with a"
echo "  one-time PIN — no account, and revocable per person."
