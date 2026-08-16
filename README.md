# Cost- and Risk-Aware GitOps Platform

**Policy-gated continuous delivery with real-time FinOps and elastic workloads on Kubernetes.**

A change to this repository is priced and checked *before* it can merge, deployed only
after it has passed, then measured against that prediction while it runs.

---

## The claim

Govern before it runs; measure while it runs.

Three things that are normally separate concerns are treated here as one system:

- **Cost** is usually discovered on next month's invoice. Here it is estimated at review
  time, from the manifest, and reconciled against live consumption after deploy.
- **Policy** is usually enforced at admission, after a change has already been merged and
  deployed. Here it blocks the merge.
- **Elasticity** is usually justified by "it scales". Here scaling events and spend are
  plotted on one axis, so the demand → replicas → cost relationship is visible rather
  than asserted.

## Why one platform instead of three projects

This design merges three earlier proposals — a FinOps CI/CD pipeline, a self-healing and
autoscaling environment, and a zero-trust network. They are not merely co-located here.
Each supplies something the others need in order to be more than a demo:

- Autoscaling without cost attribution shows replicas moving but never what they cost.
- Cost attribution without autoscaling shows a flat line with nothing to explain it.
- A pre-merge cost gate without live attribution produces an estimate nobody can check.

The join is the pricing table. `manifests/apps/cost-exporter/pricing.yaml` is read by both the
pre-merge estimate (M2) and the live cost exporter (M4), so prediction and measurement are
expressed in the same units at the same rates and can be placed on one axis and compared.
Two tables would produce two numbers that look comparable and are not — and that
comparison is the thing none of the three original proposals could make on its own.

## Architecture

Eight modules across four planes. What each one is *allowed* to do is deliberately narrow —
the sum of those boundaries is what makes the central claim defensible.

| Module | Plane | Authority |
|---|---|---|
| M1 Source & CI | Control | Builds and publishes; never deploys |
| **M2 Pre-Merge Gate** | Control | Blocks at merge; no runtime authority |
| M3 GitOps Delivery | Control | Deploys gated state only |
| M4 Cost Attribution | Data | Read-only |
| M5 Elasticity | Runtime | Owns scale, not existence |
| M6 Security Baseline | Runtime | L3/L4 default-deny; fails closed |
| M7 Observability | Data | Observation only |
| M8 Cost Intelligence | Intelligence | Advisory only; removable |

The failure philosophy splits along the same line:

- **The control plane fails safe.** M2 never merges on uncertainty — a sub-gate that
  errors is `inconclusive`, which blocks exactly like a failure. M6 fails closed.
- **The data and intelligence planes fail soft.** M4, M7 and M8 degrade to gaps and
  fallbacks rather than breaking delivery. M8 can be deleted entirely without affecting
  any other module's correctness.

Full design: [`docs/LLD.md`](docs/LLD.md).

## Status

| Phase | Module | State |
|---|---|---|
| 0 | Cluster substrate | ✅ 3-node kind cluster, Calico, enforcement verified |
| 1 | M1 Source & CI | ✅ Go workload, multi-arch build, GHCR digest-pinned publish |
| 2 | **M2 Pre-Merge Gate** | ✅ Cost + policy sub-gates, 18 rules, 20 tests, live on PRs |
| 3 | M3 GitOps Delivery | ✅ ArgoCD app-of-apps; drift reverted in ~5s, merge→cluster in 41s |
| 4 | M4 / M7 | ✅ Cost exporter, Prometheus, Grafana; prediction reconciles with measurement |
| 5 | M5 Elasticity | ✅ KEDA on queue depth; 1→6 in 15s, backlog drained, cost tracked the curve |
| 6 | M6 Security Baseline | ⬜ Default-deny, allow-lists, connectivity matrix |
| 7 | M8 Cost Intelligence | ⬜ Anomaly baseline, PR explainer |

[PR #1](https://github.com/AbdurahmanAlmehdi/cost-risk-aware-gitops-platform/pull/1) is a
deliberately non-compliant change kept open as a live demonstration of the gate.

## Quick start

Requires Docker, `kind`, `kubectl`, `helm` and Go 1.24.

```bash
make bootstrap
```

That creates the cluster, installs Calico, and **verifies that NetworkPolicy is actually
enforced** — not merely accepted. The distinction matters: the API server stores a
NetworkPolicy happily under a CNI that ignores it, so `kubectl get networkpolicy` proves
nothing. kind's default CNI does not enforce policy at all, which is why Calico is
installed at cluster-creation time rather than added later.

Hand the cluster over to Git:

```bash
make gitops-bootstrap
```

That installs ArgoCD, grants it read access to this repository, and applies the root
Application — the only object in the platform ever applied by hand. After it, adding a
workload means committing a file.

Run the gate against your working branch:

```bash
make gate BASE=main
```

Prove that Git actually governs the cluster rather than merely having populated it:

```bash
make drift-test
```

It edits a live resource the way an engineer would mid-incident and asserts the platform
reverts it unattended. Until that is demonstrated, a cluster has two sources of truth and
no way to say which is currently winning.

Check the platform's central claim — that the cost predicted before merge is the cost
actually being reserved:

```bash
make reconcile
```

M2 prices manifests in Git; M4 prices what the cluster reserved. Both read the same rate
table through the same code, so the two figures are directly comparable. Where they
disagree, something real has happened — a workload scaled outside Git, a manifest that
never landed, or an assumption in the estimate that does not hold.

```
WORKLOAD                                M2 PREDICTED   M4 MEASURED      DIFF
demo/demo-api                                   5.00          5.00        ok
demo/demo-worker                               11.73         11.73        ok
demo/redis                                      2.69          2.69        ok
observability/cost-exporter                     1.35          1.35        ok
```

Workloads reported as `not in Git` are a deliberate, honest gap: ArgoCD and the monitoring
stack are delivered from Helm charts, and the cost sub-gate can only price what it renders
from the repository.

Drive demand and watch the platform respond:

```bash
make load-test
```

It enqueues 600 jobs, then asserts all four parts of the elasticity claim separately —
demand rises, replicas follow, **the backlog actually drains**, and bounds are never
breached. The third is the one that matters: replica count alone proves the autoscaler
acted, not that the action worked.

A recorded run, with M4 measuring the cost of the same event:

```
t+ 0s   1 replica    queue 599     $11.73/month
t+15s   5 replicas   queue 547     $58.63/month
t+30s   6 replicas   queue 292     $70.36/month
t+45s   6 replicas   queue   0     backlog cleared
        1 replica    queue   0     $11.73/month
```

M2 priced this change's ceiling at **$70.36/month** before it merged. Under load, M4
measured **$70.36**. That is the whole argument of the project in one line.

### Cost is autoscaling-aware

A `ScaledObject` authorises a spending ceiling while touching no `replicas:` field, so a
gate that prices the Deployment literally reports the cost of the quietest possible moment.
Worse, omitting `maxReplicaCount` hands the decision to KEDA's default of **100 replicas**
— a one-line diff that looks like nothing and authorises roughly $1,170/month here.

So the gate prices autoscaled workloads at their ceiling, and judges two budgets rather
than one:

- **Committed spend** — the floor, what the change costs with nothing happening. Judged by
  the ordinary per-change thresholds.
- **Authorised burst** — the ceiling an autoscaler permits under load. Judged by its own
  budget, plus a ratio cap.

They have to be separate. A percentage cap tight enough to catch a careless replica bump
would block every autoscaler ever proposed, since a large idle-to-peak ratio is the entire
point of elasticity. Treating burst as free would be the opposite mistake.

## How the gate works

```
PR opened
   │
   ├─ tests ──────────── fail fast, before anything expensive
   │
   ├─ build + publish ── digest-pinned image
   │
   └─ gate
        ├─ diff ────── which kustomize roots does this PR touch?
        ├─ render ──── kustomize build at base and at head
        ├─ cost ─────┐
        ├─ policy ───┴─ evaluated independently
        └─ verdict ──── pass ⟺ both sub-gates pass
```

Some specifics that are load-bearing:

**Manifests are rendered through kustomize, not parsed.** ArgoCD renders through kustomize
too. Reading `deployment.yaml` directly would let the gate evaluate something different
from what M3 deploys the moment an overlay is involved — breaking the guarantee that what
was approved is what ships.

**The gate never calls the GitHub API.** It reads Git, decides, and prints. The workflow
delivers the verdict. So a GitHub outage can delay a report but can never turn a blocking
verdict into a passing one.

**Cost prices requests, not usage.** Requests are what the scheduler reserves and what
capacity must be planned for. Usage is unknowable before the workload has run — M4 makes
that gap visible after deploy rather than the gate pretending to predict it.

**A container with no requests is not free.** It is priced from a declared default and
flagged. Pricing it at zero would make declaring nothing the cheapest way past the cost
gate, which is exactly what the policy gate forbids.

**The rule set self-tests on startup.** The worst failure mode for a policy engine is
silence: a renamed input path or an empty policy directory makes every rule match nothing,
raise no error, and approve every pull request. Before the gate trusts the rules to accept
anything, it checks they can still reject a manifest that declares no resources, no probes
and no security context. If they cannot, it refuses to run.

## Layout

```
app/          M1 — the demo workload (API producer + queue worker)
gate/         M2 — the gate binary: diff, cost, policy, verdict, report
policies/     M2 — Rego rules (18, across resources/probes/privilege/images/network)
manifests/    M3 — desired state; the only authority on what runs
platform/     cluster substrate: kind config, Calico, ArgoCD values
gate.yaml     every value that can change a verdict, in version control
docs/         LLD and architecture decisions
```

## Honesty about the cost figures

The cluster is local. No money is spent. Every figure the platform reports is a *model* of
what the same workload would cost on a cloud provider, computed from published on-demand
rates decomposed per resource. The modelling is deterministic and auditable, and the same
rates are applied to both the prediction and the measurement — which is what makes the
comparison between them meaningful, and what the project actually claims.
