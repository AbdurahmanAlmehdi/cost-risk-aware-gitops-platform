# 3. Build a deterministic cost calculator instead of using Infracost

**Status:** Accepted
**Date:** 2026-08-14
**Departs from:** the FinOps CI/CD proposal, which named Infracost explicitly.

## Context

M2's cost sub-gate must produce a cost delta for a pull request that changes Kubernetes
manifests. Infracost is the well-known tool in this space and was named in the original
proposal, so choosing against it needs justification.

Infracost is built for Terraform. It reads infrastructure-as-code that declares *cloud
resources*, instances, disks, load balancers, and prices them from a hosted rate API. This
platform's unit of change is a Kubernetes manifest declaring *resource requests* against an
existing cluster. That is a different input at a different layer, and Infracost's support for
it is peripheral rather than central.

It also introduces two operational dependencies inside the gate: a network call and an API
key. M2 is the module whose defining property is that it fails safe on uncertainty, so any
error becomes a blocked pull request. A hosted dependency in that position converts an
outage or an expired key into a merge freeze, and puts a live network call on the critical
path of a demonstration.

## Decision

Implement the cost sub-gate as a deterministic calculator inside the gate binary. It parses
the changed manifests, computes `requests × replicas` per workload, and prices the result
from `platform/pricing/pricing.yaml`, the same table M4 uses to price live consumption.

The calculator sits behind an interface so an Infracost provider can be added later if a
reviewer requires the named tool.

## Consequences

- **The decisive benefit:** pre-merge estimates and live measurements share one rate table,
  so predicted and actual cost are directly comparable. Two pricing sources would have made
  that comparison meaningless, and that comparison is the strongest argument that these
  modules belong in one project.
- The gate is offline, deterministic, and reproducible: the same manifests and the same
  config always yield the same verdict, which is required for auditability (LLD §9.3).
- We are responsible for pricing accuracy. Mitigated by treating the rate table as a
  documented, version-controlled model with a stated source, and by recording the pricing
  version in each verdict so an old PR comment can be traced to the rates that produced it.
- The platform prices *requests*, not actual usage, at gate time, requests are what the
  scheduler reserves. Where a workload's usage diverges from its requests, M4 makes the gap
  visible after deploy; the gate cannot see it in advance.
- We do not get Infracost's cloud-resource coverage. Out of scope: this platform governs
  Kubernetes workloads, not cloud infrastructure provisioning.
