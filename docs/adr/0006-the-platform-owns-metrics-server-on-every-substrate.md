# 6. The platform owns metrics-server on every substrate

**Status:** Accepted
**Date:** 2026-08-27

## Context

`manifests/argocd/apps/metrics-server.yaml` was written against kind, which ships no
resource metrics API. On the k3s review host that assumption is wrong in a way that does
not announce itself: k3s bundles metrics-server as a built-in addon, reconciled by its own
addon controller from `/var/lib/rancher/k3s/server/manifests/metrics-server/`.

Both controllers then owned `kube-system/metrics-server`, and neither was wrong from its
own point of view:

| Object | Owner | Selector / labels |
|---|---|---|
| Deployment | k3s addon controller | pods labelled `k8s-app=metrics-server` only |
| Service | ArgoCD (Helm chart 3.13.1) | `app.kubernetes.io/name` + `app.kubernetes.io/instance` |

ArgoCD applied the chart's Service over k3s's, and the strategic merge produced the *union*
of both label sets as the live selector. A selector demanding all three labels matched a
pod carrying one. The Service had no endpoints while the Deployment sat at `1/1 Running` —
the failure presented as healthy.

ArgoCD's attempt to patch the Deployment failed differently and permanently:

```
Deployment.apps "metrics-server" is invalid: spec.selector: Invalid value:
{"app.kubernetes.io/instance","app.kubernetes.io/name","k8s-app"}: field is immutable
```

After five retries the app parked at `OutOfSync`, so nothing self-corrected.

The consequence was far larger than absent metrics. An APIService with no endpoints reports
`Available=False`, and the namespace controller refuses to finalize *any* namespace while it
cannot enumerate every API group:

```
NamespaceDeletionDiscoveryFailure=True: metrics.k8s.io/v1beta1: stale GroupVersion discovery
```

Every namespace deletion in the cluster hung indefinitely. That is what wedged `cni-verify`
in `Terminating` and stalled `make verify`, which deletes and recreates that namespace on
each run. A dormant metrics API had become a cluster-wide outage of namespace deletion, and
the symptom pointed at the CNI check rather than at metrics-server.

## Decision

k3s's bundled metrics-server is disabled, via `/etc/rancher/k3s/config.yaml`:

```yaml
disable:
  - traefik
  - metrics-server
```

The platform's own declaration is then the only one, and kind and k3s run the identical
component from the identical manifest.

The alternatives were rejected for the same underlying reason. Making the ArgoCD app
kind-only would leave the manifests substrate-conditional — the platform would describe a
different system depending on where it landed, which is precisely what GitOps is supposed
to eliminate. Overriding the chart's labels to match k3s's would restore endpoints while
leaving two controllers owning one object, so the next k3s upgrade that touches those
manifests would reintroduce the conflict silently.

Patching the live selector by hand is not a fix at all: the app runs `selfHeal: true`, so
ArgoCD reverts the patch within seconds — the same behaviour `tools/drift-test.sh` exists
to demonstrate.

`traefik` is listed alongside it because the systemd unit already passes `--disable traefik`
as a flag. Keeping the full list in the config file means one file answers what is disabled,
rather than the answer being split between a unit file and a config file.

## Consequences

Bootstrapping a k3s host now has a required step before ArgoCD is installed. It is one file,
but it is host state that lives outside Git, and forgetting it reproduces the outage above
rather than a clean error — so it is recorded here and in the manifest's own comments.

Disabling the addon makes k3s delete the objects it created, including the APIService. There
is a window between that deletion and ArgoCD's sync in which `kubectl top` and any HPA CPU
target read unknown. On this host the window was about thirty seconds. It is worth knowing
before doing this to a cluster mid-demonstration.

The failure mode is worth remembering beyond metrics-server: a single unavailable APIService
disables namespace deletion cluster-wide, and the resulting symptom appears wherever
namespaces are used rather than where the broken component is. `kubectl get apiservices` is
the fast check whenever a namespace will not terminate.
