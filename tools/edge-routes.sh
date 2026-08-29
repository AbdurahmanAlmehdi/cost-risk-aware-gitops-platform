#!/usr/bin/env bash
# Point the demonstration hostnames at the tunnel.
#
# `cloudflared tunnel route dns` resolves the record against the zone that
# ~/.cloudflared/cert.pem was authorised for, and it treats the name it is given as a record
# *within* that zone rather than as a fully-qualified name. If cert.pem was issued for a
# different zone than the hostname belongs to, it does not fail. It creates
#
#     gitops.abdurahman.ly.libyapulse.ly
#
# in the wrong zone, reports success, and the hostname you actually wanted never appears.
# That happened here, and the only symptom was three records quietly added to an unrelated
# production zone. This script therefore checks what cloudflared says it created against
# what was asked for, and stops on the first mismatch rather than repeating it three times.
set -euo pipefail

TUNNEL_NAME=${TUNNEL_NAME:-gitops-platform}
DOMAIN=${DOMAIN:-abdurahman.ly}
HOSTS=${HOSTS:-gitops argocd grafana}

for h in $HOSTS; do
  fqdn="${h}.${DOMAIN}"
  out=$(cloudflared tunnel route dns "$TUNNEL_NAME" "$fqdn" 2>&1 | tail -1)

  # cloudflared names the record it created, so the mismatch is detectable immediately
  # rather than by noticing the hostname does not resolve some minutes later.
  created=$(printf '%s' "$out" | sed -n 's/.*Added CNAME \([^ ]*\) which will route.*/\1/p')

  if [ -z "$created" ]; then
    echo "FAIL: could not tell what cloudflared created for ${fqdn}."
    echo "      Its output was: ${out}"
    exit 1
  fi

  if [ "$created" != "$fqdn" ]; then
    echo "FAIL: asked for ${fqdn}, but cloudflared created ${created}."
    echo
    echo "      cert.pem is authorised for a different zone, so the hostname was treated as"
    echo "      a subdomain of that zone. A stray record now exists at ${created} and should"
    echo "      be deleted. It points at this tunnel from a zone that should not."
    echo
    echo "      Fix: re-run 'cloudflared tunnel login' and choose ${DOMAIN}, then run this"
    echo "      again. Records already created correctly are left alone."
    exit 1
  fi

  echo "  ${fqdn} -> ${TUNNEL_NAME}"
done

echo
echo "All hostnames routed. They are public now, so confirm Cloudflare Access is in front of"
echo "them before sharing: an unauthenticated ArgoCD is worse than an unreachable one."
