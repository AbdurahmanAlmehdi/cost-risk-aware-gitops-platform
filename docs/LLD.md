# Low-Level Design Document

## Cost- and Risk-Aware GitOps Platform

**Document type:** Low-Level Design (LLD), per-module component design
**Project:** Policy-Gated Continuous Delivery with Real-Time FinOps and Elastic Workloads on Kubernetes
**Companion to:** Module Design Document (MDD), high-level
**Status:** Draft for review
**Date:** May 2026

---

## 0. How to Read This Document

The MDD defined *what* each module is and how the modules connect. This LLD goes one level deeper: for each module it specifies its internal components, the interfaces and contracts it exposes, the data structures and configuration it relies on, its control/processing logic, failure handling, and how it is tested. It stops short of source code. Every "logic" subsection describes behaviour and sequence, not implementation lines.

Conventions used throughout:
- **Interface** = a boundary another module or actor interacts with (a webhook, a status check, a CRD, a dashboard, a file in Git).
- **Contract** = the guaranteed shape and meaning of what crosses an interface.
- **Config** = values that tune behaviour without changing logic.
- All inter-module references use the MDD module IDs (M1–M8).

---

## 1. M1: Source & CI

### 1.1 Internal Components
- **Repository layout component**: defines the on-disk structure: application source, a `manifests/` tree (one directory per deployable workload), and a `policies/` tree consumed by M2.
- **Build component**: compiles/containerizes the application.
- **Test component**: runs unit and integration tests.
- **Publish component**: pushes the built image to the registry and records its immutable digest.
- **Trigger component**: emits events that start M2 (on PR) and inform M3 (on merge to the tracked branch).

### 1.2 Interfaces & Contracts
- **Inbound:** Git push / pull-request events from developers.
- **Outbound to M2:** a PR event carrying the PR identifier, the base and head commit references, and the list of changed manifest paths.
- **Outbound to registry:** an image reference of the form `registry/app@sha256:digest` (digest-pinned, never floating tags).
- **Contract:** the manifest set in Git is the *only* authority on desired state. The image referenced inside a manifest must be digest-pinned so that what M2 evaluates is byte-identical to what M3 deploys.

### 1.3 Data Structures & Config
- **Pipeline definition**: declarative CI workflow: trigger conditions, ordered jobs (build → test → publish), and the gate-invocation step that hands control to M2.
- **Config:** target branch name, registry location, test command set, image-naming convention.

### 1.4 Control / Processing Logic
1. Receive Git event.
2. If it is a PR, run build + test, publish a candidate image, then invoke M2 with the changed-manifest list.
3. If it is a merge to the tracked branch, ensure the image is published and rely on M3 to reconcile (M1 itself does not deploy).
4. Surface job status back onto the PR.

### 1.5 Failure Handling
- Build or test failure fails the pipeline and the PR check before M2 is ever invoked (cheap failures first).
- Registry-push failure is retried with backoff; persistent failure blocks the PR with a clear cause.

### 1.6 Testing Strategy
- Unit tests on the application.
- A pipeline smoke test: a trivial PR must traverse build → test → publish → gate-invocation end to end.

---

## 2. M2: Pre-Merge Gate (core)

### 2.1 Internal Components
- **Diff resolver**: determines which manifests changed in the PR and loads both old and new versions.
- **Cost sub-gate**: produces a cost delta for the change.
- **Policy sub-gate**: evaluates the changed manifests against codified rules.
- **Verdict aggregator**: combines both sub-gate results into a single pass/fail.
- **Reporter**: writes the inline PR comment and sets the status check.
- **Explainer hook**: on failure, hands the failing report and manifests to M8b.

### 2.2 Interfaces & Contracts
- **Inbound from M1:** PR identifier, commit refs, changed-manifest list.
- **Outbound to PR:** a single pass/fail status check, plus a structured comment containing the cost delta and the list of policy violations (each with rule ID, offending resource, and severity).
- **Outbound to M8b:** the failing report and the changed manifests (only on failure).
- **Contract:** the gate is **advisory-and-blocking at merge time only**. It has no runtime authority. A PR passes if and only if *both* sub-gates pass.

### 2.3 Data Structures & Config
- **Cost-delta record**: baseline cost, projected cost, delta (absolute and percentage), and the threshold it was compared against.
- **Policy-violation record**: rule ID, resource kind/name, severity (block vs. warn), human-readable message.
- **Config:**
  - Cost: budget threshold model (fixed per-PR ceiling *or* percentage delta vs. baseline, open question from MDD), and which violations block vs. warn.
  - Policy: the rule set (required resource requests/limits; required liveness/readiness probes; no overly permissive network or privilege settings).

### 2.4 Control / Processing Logic
1. Diff resolver loads changed manifests.
2. Cost and policy sub-gates run **in parallel** (independent concerns, independent failure semantics).
3. Cost sub-gate computes the delta and compares to threshold.
4. Policy sub-gate evaluates rules and collects violations.
5. Verdict aggregator: fail if cost exceeds a blocking threshold OR any blocking policy violation exists; otherwise pass (warnings still reported).
6. Reporter posts the combined comment and sets the check.
7. On failure, the explainer hook invokes M8b (non-blocking. M8b's availability never affects the verdict).

### 2.5 Failure Handling
- A sub-gate that errors (as opposed to returning a clean fail) is treated as a **blocking inconclusive** result, the platform fails safe, never merges on uncertainty.
- M8b being slow or unavailable does not delay or alter the verdict; the explanation is best-effort enrichment.

### 2.6 Testing Strategy
- Golden-manifest fixtures: known-good and known-bad manifests with expected verdicts.
- A deliberately non-compliant PR (resource spike + missing probe) must be blocked. This doubles as the demo case.
- Threshold boundary tests (just-under vs. just-over budget).

---

## 3. M3: GitOps Delivery

### 3.1 Internal Components
- **Repository watcher**: observes the tracked branch for changes.
- **Reconciler**: computes the difference between desired (Git) and live (cluster) state and applies it.
- **Drift detector**: continuously compares live state to Git and corrects divergence.
- **Health reporter**: exposes per-application sync and health status.

### 3.2 Interfaces & Contracts
- **Inbound:** desired state (manifests already validated by M2) on the tracked branch.
- **Outbound:** applied cluster state; a sync/health status surface consumed by operators and M7.
- **Contract:** M3 deploys **only** what already passed M2; it never re-evaluates cost or policy. Delivery is pull-based, the cluster reconciles toward Git, M1 holds no cluster credentials.

### 3.3 Data Structures & Config
- **Application definition**: maps a Git path (a manifest directory) to a target namespace and sync policy.
- **Config:** sync mode (automated), self-heal enabled, prune-orphaned-resources policy, target cluster/namespace mapping.

### 3.4 Control / Processing Logic
1. Watcher detects a new commit on the tracked branch.
2. Reconciler diffs desired vs. live and applies the delta.
3. Drift detector runs on an interval and on event; any manual change to the live cluster is reverted to match Git.
4. Health reporter publishes per-app status.

### 3.5 Failure Handling
- A sync that produces unhealthy resources is reported as degraded and (per policy) can auto-rollback to the last healthy revision.
- Reconciliation is idempotent, repeated application of the same desired state is a no-op.

### 3.6 Testing Strategy
- Merge-to-deploy test: an approved change appears in the cluster with no manual step.
- Drift test: a manual edit to a live resource is detected and reverted.

---

## 4. M4: Cost Attribution

### 4.1 Internal Components
- **Usage collector**: reads live CPU, memory, and storage consumption per workload.
- **Pricing mapper**: applies a standard cloud pricing model to usage.
- **Allocator**: attributes cost to deployment and namespace dimensions.
- **Series writer**: emits cost as a time series for M7 and M8a.

### 4.2 Interfaces & Contracts
- **Inbound:** live resource metrics from the cluster.
- **Outbound:** per-workload and per-namespace cost time series.
- **Contract:** **read-only**. M4 observes and accounts, never mutates workloads. Cost figures are modelled from standard pricing and are explicitly approximate on a local cluster.

### 4.3 Data Structures & Config
- **Cost sample**: timestamp, workload identity (namespace + deployment), resource dimension (cpu/mem/storage), usage quantity, unit price, computed cost.
- **Config:** the pricing table (rates per resource unit), sampling interval, allocation granularity.

### 4.4 Control / Processing Logic
1. Collector samples usage on the configured interval.
2. Pricing mapper converts each usage quantity to cost via the pricing table.
3. Allocator rolls costs up to deployment and namespace levels.
4. Series writer appends to the cost time series.

### 4.5 Failure Handling
- A missed sample creates a gap, not a crash; downstream consumers tolerate gaps.
- An unknown resource type uses a configurable default rate and is flagged rather than silently dropped.

### 4.6 Testing Strategy
- Pricing-mapper unit tests: known usage × known rate = expected cost.
- Allocation test: per-workload costs sum to the namespace total.

---

## 5. M5: Elasticity

### 5.1 Internal Components
- **Signal source**: exposes the demand metric (queue depth or request rate).
- **Scaler controller**: event-driven scaling decision-maker (KEDA), with metric-based HPA as fallback.
- **Replica governor**: applies replica-count changes within configured bounds.
- **Event emitter**: publishes scale-up/scale-down events for M7.

### 5.2 Interfaces & Contracts
- **Inbound:** the demand signal; resource metrics.
- **Outbound:** replica-count changes on the target workload; scaling events to M7.
- **Contract:** M5 owns *scale*, not *existence*. It never decides whether a workload should exist (that is M1–M3). It operates only on workloads M3 has deployed.

### 5.3 Data Structures & Config
- **Scaling rule**: target workload, trigger metric, threshold, min/max replicas, cooldown/stabilization window.
- **Config:** the trigger definition, bounds, and cooldown to prevent flapping.

### 5.4 Control / Processing Logic
1. Scaler reads the demand signal against the threshold.
2. If demand exceeds the threshold and replicas < max, scale up; if below and replicas > min, scale down (respecting cooldown).
3. Replica governor applies the change.
4. Event emitter records the scaling event with before/after counts.

### 5.5 Failure Handling
- If the event-driven signal is unavailable, fall back to metric-based (HPA) scaling on CPU/memory.
- Cooldown windows prevent rapid oscillation under noisy signals.

### 5.6 Testing Strategy
- Load test: synthetic load drives a verifiable scale-up, then scale-down on release.
- Bounds test: replicas never exceed max or drop below min.

---

## 6. M6: Security & Network Baseline

### 6.1 Internal Components
- **Policy definitions**: the allow-list rules describing which workloads may talk to which.
- **Enforcement point**: the CNI that applies default-deny and the explicit allows at L3/L4.
- **Verification probe**: a test harness that confirms allowed paths work and denied paths are blocked.

### 6.2 Interfaces & Contracts
- **Inbound:** network policy definitions (themselves version-controlled and gated through M2).
- **Outbound:** enforced allow/deny communication paths.
- **Contract:** L3/L4 isolation only, default-deny with explicit allow-lists. Full service-mesh mTLS is **out of scope** and noted as a future extension.

### 6.3 Data Structures & Config
- **Network policy**: selector for the target workload(s), allowed ingress sources, allowed egress destinations, ports.
- **Config:** namespace isolation defaults; the per-workload allow-lists.

### 6.4 Control / Processing Logic
1. Default-deny is applied cluster-wide as the baseline.
2. Explicit allow-lists open only the declared dependency paths.
3. The verification probe periodically asserts both directions (allowed succeeds, denied fails).

### 6.5 Failure Handling
- A workload missing an allow-list rule fails *closed* (cannot communicate) rather than open, safe by default.
- Policy changes flow through M2, so an overly permissive rule is caught at the gate.

### 6.6 Testing Strategy
- Connectivity matrix test: for each pair of workloads, assert the expected allow/deny outcome.

---

## 7. M7: Observability

### 7.1 Internal Components
- **Scraper**: collects cluster and application metrics.
- **Store**: time-series storage for metrics.
- **Dashboard set**: the visualization surfaces (cost, scaling, health, anomalies).
- **Correlation view**: the panel that overlays cost (M4) against scaling events (M5) to make the demand→scale→spend relationship visible.

### 7.2 Interfaces & Contracts
- **Inbound:** metrics from the cluster; cost from M4; scaling events from M5; anomaly flags from M8a.
- **Outbound:** dashboards, the platform's primary demonstration surface.
- **Contract:** observation only, no control authority.

### 7.3 Data Structures & Config
- **Metric series**: standard time-series (name, labels, samples).
- **Dashboard definition**: panels, queries, layout.
- **Config:** scrape targets and interval; dashboard provisioning definitions.

### 7.4 Control / Processing Logic
1. Scraper pulls metrics on an interval.
2. Store retains them for the configured window.
3. Dashboards query the store; the correlation view aligns cost and scaling on a shared time axis.

### 7.5 Failure Handling
- A scrape failure leaves a gap; dashboards render available data rather than failing whole.

### 7.6 Testing Strategy
- Dashboard provisioning test: dashboards load with expected panels against seeded data.
- Correlation test: a known load episode shows aligned cost and scaling movement.

---

## 8. M8: Cost Intelligence (advisory AI plane)

M8 is split into two independent sub-components. **Both are strictly advisory**: no output blocks a deploy, alters a replica count, or overrides M2. Each has a mandatory deterministic fallback, so M8 can be removed entirely without affecting any other module's correctness.

### 8.1 M8a. Cost Anomaly Detection (primary)

**Internal components**
- **Feature builder**: turns the M4 cost time series into per-workload features (recent trend, seasonality, baseline).
- **Detector**: the layered decision: a deterministic statistical baseline plus an optional trained model.
- **Forecaster**: projects expected near-term spend per workload.
- **Flag emitter**: raises anomaly flags to M7.

**Interfaces & contracts**
- **Inbound:** historical and live cost/usage series from M4.
- **Outbound:** spend forecasts and anomaly flags surfaced via M7.
- **Contract:** advisory only; raises alerts, never acts.

**Data structures & config**
- **Anomaly flag**: workload identity, observed value, expected range, deviation magnitude, confidence, detector source (baseline vs. model).
- **Config:** baseline sensitivity (e.g., deviation multiplier), forecast horizon, model-enable switch, confidence floor below which the model output is ignored in favour of the baseline.

**Control / processing logic**
1. Feature builder assembles per-workload features from the cost series.
2. The deterministic baseline (e.g., moving-average with a MAD-style outlier bound) always runs and is the source of truth on its own.
3. If the model is enabled and confident above the floor, its output augments the baseline; otherwise the baseline stands alone.
4. Flags and forecasts are emitted to M7. The platform's zombie-resource detection consumes these to become smarter than a static threshold.

**Failure handling**
- Model unavailable, untrained, or low-confidence → silently fall back to the statistical baseline. Detection never stops.
- Insufficient history → baseline operates in a conservative mode; no model claims are made.

**Testing strategy**
- Inject a synthetic cost spike and assert it is flagged by the baseline alone (model-independent).
- Confirm graceful degradation: with the model disabled, detection still functions.

### 8.2 M8b. Pull-Request Explanation (secondary)

**Internal components**
- **Context assembler**: packages the failing M2 report and the changed manifest into a prompt.
- **Explanation generator**: the language model producing the plain-language reason and suggested fix.
- **Comment poster**: writes the result back to the PR.

**Interfaces & contracts**
- **Inbound:** the failing gate report and changed manifests from M2 (failure path only).
- **Outbound:** an inline PR comment with explanation and a concrete suggested fix.
- **Contract:** advisory only; the merge decision stays entirely with M2's deterministic logic.

**Data structures & config**
- **Explanation record**: referenced rule/violation, plain-language cause, suggested manifest change.
- **Config:** model endpoint, prompt template, an on/off switch.

**Control / processing logic**
1. M2 fails a PR and hands over the report and manifests.
2. Context assembler builds the prompt.
3. Generator produces explanation + suggested fix.
4. Poster adds it as an inline comment alongside M2's structured report.

**Failure handling**
- Generator slow or unavailable → no comment is posted; M2's structured report stands on its own. The verdict is never delayed or changed.

**Testing strategy**
- Given a known violation, assert an explanation is posted and references the correct rule.
- Assert that disabling M8b leaves M2's verdict and report fully intact.

---

## 9. Cross-Cutting Concerns

### 9.1 Configuration Management
All tunable values (thresholds, pricing table, scaling bounds, policy rules, AI switches) live in version-controlled configuration, themselves subject to M2's gates where they affect deployment. No behaviour-altering value is hard-coded.

### 9.2 Failure Philosophy
- **Control plane fails safe:** M2 never merges on uncertainty; M6 fails closed.
- **Data and intelligence planes fail soft:** M4, M7, and M8 degrade gracefully (gaps, fallbacks) rather than breaking delivery.
- This split follows directly from the plane model: things that decide what runs must be strict; things that observe or advise must never become a single point of failure.

### 9.3 Idempotency & Determinism
M3 reconciliation is idempotent; M2's verdict is deterministic for a given manifest set and config; M8's advisory output never feeds back into a deterministic decision. Together these keep the system's behaviour reproducible and auditable.

### 9.4 Security of the Pipeline Itself
M1 holds no cluster credentials (pull-based delivery). Policy and network definitions are version-controlled and pass through the same M2 gate as application changes, so the platform governs changes to itself the same way it governs application changes.

---

## 10. Traceability to the MDD

| MDD module | LLD section | Plane | Authority |
|------------|-------------|-------|-----------|
| M1 Source & CI | §1 | Control | Builds/publishes; never deploys |
| M2 Pre-Merge Gate | §2 | Control | Blocks at merge; no runtime authority |
| M3 GitOps Delivery | §3 | Control | Deploys gated state only |
| M4 Cost Attribution | §4 | Data | Read-only |
| M5 Elasticity | §5 | Runtime | Owns scale, not existence |
| M6 Security Baseline | §6 | Runtime | L3/L4 default-deny; fails closed |
| M7 Observability | §7 | Data | Observation only |
| M8 Cost Intelligence | §8 | Intelligence | Advisory only; removable |

Every module's authority statement is intentionally narrow, the sum of these boundaries is what lets the platform make its central claim (govern before it runs, measure while it runs) without any single component being able to undermine it.
