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

# --- reviewer password -------------------------------------------------------
if [ -z "${REVIEWER_PASSWORD:-}" ]; then
  REVIEWER_PASSWORD=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 20)
  GENERATED=1
fi

# Hashed with Caddy's own bcrypt implementation, so the hash is guaranteed to be in the
# format its basic_auth directive expects.
HASH=$(docker run --rm caddy@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d \
  caddy hash-password --plaintext "$REVIEWER_PASSWORD" 2>/dev/null | tail -1)

kubectl -n "$NS" create secret generic edge-basic-auth \
  --from-literal=reviewer-hash="$HASH" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
echo "basic-auth credential installed"

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

if [ "${GENERATED:-0}" = "1" ]; then
  echo
  echo "  reviewer password: ${REVIEWER_PASSWORD}"
  echo
  echo "  Username is 'reviewer'. This is the only time it is printed — only the hash is"
  echo "  stored. Share it with reviewers over something other than the link itself."
fi
