# 4. Scale on a shared queue rather than an in-process signal

**Status:** Accepted
**Date:** 2026-08-14

## Context

M5 scales the worker tier on demand. The demand signal could be per-process (each replica
reporting its own backlog or CPU) or shared (all replicas drawing from one queue).

A per-process signal makes the autoscaling claim unfalsifiable. If each replica reports a
number about itself, adding replicas changes the *set* of reporters rather than the
quantity of work, and the aggregate moves for reasons that have nothing to do with whether
scaling helped. A demonstration built on it cannot distinguish "scaling absorbed the load"
from "scaling changed how load is counted".

## Decision

Introduce Redis as a shared work queue. The API pushes jobs, workers block-pop from it, and
both KEDA and the application read demand from the same key (`jobs:pending`).

## Consequences

- Queue depth is a real backlog: adding replicas must actually reduce it, so the scaling
  claim can fail and is therefore worth testing.
- The queueing delay (`app_job_wait_seconds`) becomes the honest measure of whether scaling
  helped. Job *duration* stays flat no matter how backed up the system is, so it would have
  looked healthy during a total collapse.
- One design serves four modules: M5 gets a shared signal, M6 gets a backend worth
  protecting with an allow-list, M4 gets a second workload with a different cost profile,
  and the taint/affinity requirement gets a stateful workload to isolate.
- Adds a stateful component. Contained by pinning Redis to the tainted platform node, which
  application workloads do not tolerate — so M5 can scale workers from 1 to 20 without ever
  contending with the queue they depend on.
- Redis runs single-replica with persistence disabled. It is working state, not a database:
  a second replica would be a second queue and make depth ambiguous, and restoring a stale
  backlog on restart would misreport demand to M5. Replication would need Sentinel or
  Cluster, which is out of scope.
