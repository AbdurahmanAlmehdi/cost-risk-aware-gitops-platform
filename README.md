# Cost- and Risk-Aware GitOps Platform

Policy-gated continuous delivery with real-time FinOps and elastic workloads on Kubernetes.

The platform's claim is narrow and testable: **govern a change before it runs, and measure
it while it runs.** A pull request that would raise infrastructure cost beyond a budget, or
that violates a codified safety rule, is blocked at merge time. Everything that survives the
gate is delivered by pull-based GitOps, priced continuously against the same rate table the
gate used, scaled on real demand, and isolated by a default-deny network baseline.

## Why one platform instead of three projects

This design merges three earlier proposals — a FinOps CI/CD pipeline, a self-healing and
autoscaling environment, and a zero-trust network. They are not merely co-located here; each
supplies something the others need to be more than a demo:

- Autoscaling without cost attribution shows replicas moving but never what they cost.
- Cost attribution without autoscaling shows a flat line with nothing to explain it.
- A pre-merge cost gate without live attribution produces an estimate nobody can check.

Joining them closes that last loop: **M2 estimates a change's cost before merge and M4
measures the same workload after deploy, both from one version-controlled pricing table**, so
prediction and reality can be placed on a single axis and compared.

## Architecture

Modules are grouped into planes by how they are allowed to fail:

| Plane | Modules | Failure behaviour |
|---|---|---|
| Control | M1 Source & CI, M2 Pre-Merge Gate, M3 GitOps Delivery | **Fails safe** — never merges or deploys on uncertainty |
| Runtime | M5 Elasticity, M6 Security Baseline | M6 fails **closed**; M5 falls back to metric-based scaling |
| Data | M4 Cost Attribution, M7 Observability | **Fails soft** — gaps, never outages |
| Intelligence | M8a Anomaly Detection, M8b PR Explanation | **Advisory only** — removable without affecting correctness |

That split is the design's load-bearing idea. Anything that decides *what runs* must be
strict; anything that *observes or advises* must never become a single point of failure. M8
can be deleted entirely and every other module still behaves correctly.

Full specification: [`docs/LLD.md`](docs/LLD.md). Decisions and their rationale:
[`docs/adr/`](docs/adr/).

## Repository layout

```
app/          M1 — demo workload: API producer + worker consumer over a shared queue
gate/         M2 — the pre-merge gate (cost sub-gate + policy sub-gate + verdict)
policies/     M2 — codified rules the policy sub-gate evaluates
manifests/    M3 — desired state; the only authority on what runs
platform/     cluster substrate: kind, Calico, pricing table, observability, KEDA
exporter/     M4 — live cost attribution
tools/        verification scripts (CNI enforcement, connectivity matrix, load)
docs/         LLD, ADRs, demo script
```

## Getting started

Requires Docker, `kind`, `kubectl`, `helm`, and Go 1.24+.

```bash
make bootstrap
```

This creates a 3-node kind cluster, installs Calico, and then **verifies that the CNI
actually enforces NetworkPolicy** rather than merely accepting it — the API server will
happily store a policy that an inert CNI ignores, so enforcement is tested, not assumed.

Deploy the demo workload:

```bash
kubectl apply -f manifests/platform/namespaces.yaml
kubectl apply -k manifests/apps/redis
kubectl apply -k manifests/apps/demo-api
kubectl apply -k manifests/apps/demo-worker
```

Drive load and watch the queue drain:

```bash
curl -X POST "http://localhost:8081/api/jobs?count=50&duration_ms=500"
curl "http://localhost:8081/api/queue"
```

`make help` lists every target.

## Status

| Phase | Module | State |
|---|---|---|
| 0 | Cluster substrate | **Done** — 3 nodes, Calico, enforcement verified |
| 1 | M1 workload + CI | Workload done; CI pipeline pending |
| 2 | M2 pre-merge gate | Configuration defined; implementation pending |
| 3 | M3 GitOps delivery | Pending |
| 4 | M4 / M7 cost + observability | Pending |
| 5 | M5 elasticity | Pending |
| 6 | M6 network baseline | Foundation verified; policies pending |
| 7 | M8 cost intelligence | Pending |
