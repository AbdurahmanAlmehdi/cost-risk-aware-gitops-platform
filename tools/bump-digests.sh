#!/usr/bin/env bash
# Rewrite the image digests the manifests pin, from what CI has just published.
#
# This closes the gap that made application code the one change type that never reached the
# cluster. A manifest change merges and ArgoCD applies it; a Go change merges, CI publishes
# a new image, and nothing updates the digest that would deploy it, so a new image existed
# that nothing referenced.
#
# The fix is deliberately NOT ArgoCD Image Updater writing to the cluster or to main. That
# would make the one change type that alters what actually runs the one change nobody
# reviews, which contradicts the platform's central claim. Instead this produces a diff,
# and the diff goes through the gate like anything else.
#
# Editing is line-based rather than YAML-aware on purpose: a YAML round-trip drops comments,
# and the comments in these files carry the reasoning for why the digest is pinned at all.
# The manifests keep `digest:` on a single line for exactly this reason.
set -euo pipefail

DIGEST_DIR=${1:-}
if [ -z "$DIGEST_DIR" ] || [ ! -d "$DIGEST_DIR" ]; then
  echo "usage: bump-digests.sh <dir>" >&2
  echo "  <dir> holds one file per image, named after the image, containing its digest." >&2
  exit 2
fi

# Only images published from this repository are ever rewritten. Third-party images, redis,
# caddy, cloudflared, are pinned to digests chosen deliberately, and an automated bump has
# no business moving them: nothing here builds them, so nothing here knows what changed.
REGISTRY_PREFIX=${REGISTRY_PREFIX:-ghcr.io/abdurahmanalmehdi/cost-risk-aware-gitops-platform}
MANIFEST_GLOB=${MANIFEST_GLOB:-manifests/apps/*/kustomization.yaml}

changed_any=0

for digest_file in "$DIGEST_DIR"/*; do
  [ -f "$digest_file" ] || continue
  image=$(basename "$digest_file")
  digest=$(tr -d '[:space:]' < "$digest_file")

  # A malformed or empty digest must stop the run, not get written into a manifest. A
  # manifest pinned to nonsense fails at pull time, long after the merge that caused it.
  case "$digest" in
    sha256:[0-9a-f]*)
      [ ${#digest} -eq 71 ] || { echo "FAIL: $image digest is not 64 hex characters: $digest" >&2; exit 1; } ;;
    *)
      echo "FAIL: $image has no usable digest: '$digest'" >&2; exit 1 ;;
  esac

  target="$REGISTRY_PREFIX/$image"

  for manifest in $MANIFEST_GLOB; do
    [ -f "$manifest" ] || continue
    grep -q "newName: $target\$" "$manifest" || continue

    current=$(awk -v t="$target" '
      $1 == "newName:" && $2 == t { found = 1; next }
      found && $1 == "digest:"    { print $2; exit }
    ' "$manifest")

    if [ "$current" = "$digest" ]; then
      echo "  unchanged  $manifest ($image)"
      continue
    fi

    # Anchored on the newName line and applied to the first digest that follows it, so a
    # file carrying more than one image entry updates only the entry that matched.
    awk -v t="$target" -v d="$digest" '
      $1 == "newName:" && $2 == t { found = 1 }
      found && $1 == "digest:"    { sub(/sha256:[0-9a-f]*/, d); found = 0 }
      { print }
    ' "$manifest" > "$manifest.tmp"

    # Prove the rewrite landed before replacing the original. Without this the script could
    # report success having silently written the file back unchanged.
    if ! grep -q "digest: $digest\$" "$manifest.tmp"; then
      rm -f "$manifest.tmp"
      echo "FAIL: could not rewrite the digest in $manifest" >&2
      exit 1
    fi
    mv "$manifest.tmp" "$manifest"
    echo "  bumped     $manifest ($image)"
    echo "             $current"
    echo "          -> $digest"
    changed_any=1
  done
done

if [ "$changed_any" -eq 0 ]; then
  echo "Every manifest already pins the published digest. Nothing to do."
fi
