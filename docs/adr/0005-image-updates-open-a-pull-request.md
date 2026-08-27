# 5. Image updates open a pull request rather than writing to the cluster

**Status:** Accepted
**Date:** 2026-08-27

## Context

The manifests pin images to digests, and CI publishes a new digest on every merge. Nothing
connected the two. The result was a gap that only affected one kind of change:

| Change | Result |
|---|---|
| A manifest (YAML) | merge → ArgoCD pulls → deployed |
| Application code (Go) | merge → CI publishes a new image → **nothing deploys it** |

So a new image existed that nothing referenced. This is a direct consequence of digest
pinning — Git is the authority, and nothing updated the digest in Git. It is confusing in
exactly the wrong moment: someone edits Go during a demonstration, watches nothing happen,
and the honest explanation is that the platform is working as designed.

The obvious fix is ArgoCD Image Updater, which watches the registry and writes the new
digest either to the cluster or straight to `main`.

## Decision

CI opens a **pull request** that bumps the digest. The bump then passes through M2 like any
other change.

Image Updater was rejected because of what it implies rather than how it works. Writing
straight to the cluster or to `main` would make the one change type that alters what
actually runs the one change nobody reviews — the precise inverse of the platform's central
claim, that a change is priced and checked *before* it can merge. A platform that governs
YAML strictly while letting the bytes that actually execute change unreviewed would be
governing the label rather than the thing.

The rewrite is line-based (`tools/bump-digests.sh`) rather than YAML-aware, because a YAML
round-trip drops comments and the comments in these files carry the reasoning for why the
digest is pinned at all. The manifests keep `digest:` on a single line for this reason.

## Consequences

- A code change now takes two merges to reach the cluster: the code, then the bump. That is
  the cost of the bump being reviewable, and it is the intended trade.
- **A separate token is required.** A pull request opened with the default `GITHUB_TOKEN`
  does not trigger workflows — GitHub suppresses that to prevent recursion — so the bump
  would arrive carrying no gate verdict, which is the one kind of change this platform must
  never produce. The job therefore requires an `IMAGE_BUMP_TOKEN` secret and **fails loudly
  when it is absent**, printing the exact diff it would have proposed so the bump can still
  be made by hand. It never opens an ungated pull request.
- The workflow's own `GITHUB_TOKEN` stays `contents: read`. The write capability lives only
  in the separate token, so the workflow that runs the gate cannot push to the repository it
  is gating. `main` has no branch protection, so this restraint is doing real work rather
  than being redundant with it.
- Only images published by this repository are rewritten. Third-party pins — redis, caddy,
  cloudflared — are chosen deliberately, and nothing here builds them, so nothing here knows
  what changed.
- One reusable branch is force-pushed, so a run of merges keeps a single pull request
  current rather than accumulating a queue of stale ones.
- The loop converges: the bump changes only manifests, so the rebuild produces the same
  digest and the next run finds nothing to do.
