# 2. Replace kind's default CNI with Calico, at cluster-creation time

**Status:** Accepted
**Date:** 2026-08-14

## Context

M6's security baseline is default-deny with explicit allow-lists, enforced at L3/L4. kind
ships with kindnet, which **does not implement NetworkPolicy**.

This creates a specific and dangerous failure mode. The Kubernetes API server validates and
stores a `NetworkPolicy` object regardless of whether the installed CNI can act on it. Under
kindnet, `kubectl get networkpolicy` lists the policies, `kubectl describe` shows the
expected rules, and the manifests pass review — while all traffic continues to flow. The
platform would report a security posture it does not have, which is worse than having no
policy at all, because it is believed.

Switching CNI requires `disableDefaultCNI: true` in the kind configuration, which is only
honoured at cluster creation. Deferring this decision to the phase where M6 is implemented
would mean rebuilding the cluster and everything deployed on it.

## Decision

Create the cluster with the default CNI disabled and install Calico immediately afterwards.
Additionally, `tools/verify-cni.sh` runs as part of `make cni` and **proves enforcement**:
it asserts that traffic flows with no policy, is blocked under default-deny, and flows again
under an explicit allow. A CNI that ignores policy fails the middle assertion.

## Consequences

- Nodes stay `NotReady` between cluster creation and CNI installation. This is expected, and
  `make bootstrap` sequences the two so it is never seen as a fault.
- Bootstrap is roughly two minutes slower while Calico programmes the dataplane.
- Calico adds ~10 pods to the cluster, which on an 8 GB Docker allocation is a real cost.
  Accepted: M6 is not demonstrable without an enforcing CNI.
- The security claim is now falsifiable — the verification is a test that can fail, not an
  assertion in a document.

Applies as of Calico v3.32, where operator CRDs ship in `operator-crds.yaml` and must be
applied before `tigera-operator.yaml`; applying the `Installation` CR first fails with a
bare `NotFound`.
