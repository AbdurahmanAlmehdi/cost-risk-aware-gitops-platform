// Package attribution turns live cluster metrics into per-workload cost.
//
// It reports every workload on two bases:
//
//   - requested — the capacity the scheduler reserved. This is what M2 priced before the
//     change merged, so it is directly comparable to the pre-merge estimate.
//   - actual — what the workload genuinely consumed.
//
// Reporting both is the point of the module. One number alone answers "what does this
// cost"; the pair answers "was the estimate right, and how much of what we are paying for
// is actually being used" — which is the question a FinOps platform exists to answer, and
// the one neither a pre-merge gate nor a usage dashboard can answer by itself.
package attribution

import (
	"context"
	"fmt"
	"sort"

	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"

	"github.com/AbdurahmanAlmehdi/gitops-platform/exporter/internal/promapi"
)

const bytesPerGiB = 1024 * 1024 * 1024

// workloadMapping joins pod-level metrics up to the workload that owns them.
//
// Pods are ephemeral and their names contain generated suffixes, so per-pod cost is
// unusable for budgeting: the series disappears on every rollout and every scale event.
// Cost has to be attributed to something durable, which is the workload.
//
// Deployments own pods through an intermediate ReplicaSet, so the ReplicaSet's generated
// suffix is stripped to recover the Deployment name. StatefulSets, DaemonSets and Jobs own
// their pods directly and need no such surgery.
const workloadMapping = `(
  label_replace(kube_pod_owner{owner_kind="ReplicaSet"}, "workload", "$1", "owner_name", "^(.+)-[^-]+$")
  or
  label_replace(kube_pod_owner{owner_kind=~"StatefulSet|DaemonSet|Job"}, "workload", "$1", "owner_name", "^(.+)$")
)`

// Basis distinguishes reserved capacity from consumption.
type Basis string

const (
	Requested Basis = "requested"
	Actual    Basis = "actual"
)

// Usage is one workload's resource footprint on one basis.
type Usage struct {
	Namespace string
	Workload  string
	Basis     Basis
	CPUCores  float64
	MemoryGiB float64
}

// Cost pairs a footprint with its price.
type Cost struct {
	Usage
	HourlyUSD  float64
	MonthlyUSD float64
}

type Collector struct {
	prom  *promapi.Client
	table *pricing.Table
	// window is the averaging period for CPU rate. Too short and the figure whipsaws with
	// every burst; too long and a scale event takes minutes to appear on the dashboard,
	// which would break the demand→scale→spend correlation M7 is meant to make visible.
	window string
}

func New(prom *promapi.Client, table *pricing.Table, window string) *Collector {
	return &Collector{prom: prom, table: table, window: window}
}

func (c *Collector) queries() map[string]struct {
	basis    Basis
	resource string
} {
	return map[string]struct {
		basis    Basis
		resource string
	}{
		// Actual CPU, in cores. `container!=""` excludes the pod-level cgroup rollup,
		// which would otherwise double-count every container in the pod.
		fmt.Sprintf(`sum by (namespace, workload) (
  rate(container_cpu_usage_seconds_total{container!="", container!="POD"}[%s])
  * on (namespace, pod) group_left(workload) %s
)`, c.window, workloadMapping): {Actual, "cpu"},

		// Actual memory, in bytes. Working set rather than RSS: it is what the kubelet
		// uses for eviction decisions, so it is the number that determines how much
		// memory a workload genuinely needs to be given.
		fmt.Sprintf(`sum by (namespace, workload) (
  container_memory_working_set_bytes{container!="", container!="POD"}
  * on (namespace, pod) group_left(workload) %s
)`, workloadMapping): {Actual, "memory"},

		fmt.Sprintf(`sum by (namespace, workload) (
  kube_pod_container_resource_requests{resource="cpu"}
  * on (namespace, pod) group_left(workload) %s
)`, workloadMapping): {Requested, "cpu"},

		fmt.Sprintf(`sum by (namespace, workload) (
  kube_pod_container_resource_requests{resource="memory"}
  * on (namespace, pod) group_left(workload) %s
)`, workloadMapping): {Requested, "memory"},
	}
}

// Collect gathers every workload's cost on both bases.
func (c *Collector) Collect(ctx context.Context) ([]Cost, error) {
	type key struct {
		namespace, workload string
		basis               Basis
	}
	usage := map[key]*Usage{}

	for query, meta := range c.queries() {
		samples, err := c.prom.Query(ctx, query)
		if err != nil {
			// One failed query must not discard the three that succeeded — a partial
			// picture is more useful than none, and M4 is a fail-soft data-plane module
			// (LLD §9.2). The caller records the error so the gap is visible.
			return nil, fmt.Errorf("query %s/%s: %w", meta.basis, meta.resource, err)
		}

		for _, s := range samples {
			ns, workload := s.Labels["namespace"], s.Labels["workload"]
			if ns == "" || workload == "" {
				continue
			}
			k := key{ns, workload, meta.basis}
			if usage[k] == nil {
				usage[k] = &Usage{Namespace: ns, Workload: workload, Basis: meta.basis}
			}
			switch meta.resource {
			case "cpu":
				usage[k].CPUCores = s.Value
			case "memory":
				usage[k].MemoryGiB = s.Value / bytesPerGiB
			}
		}
	}

	costs := make([]Cost, 0, len(usage))
	for _, u := range usage {
		// Storage is priced by the gate from volume claims but is not attributed live:
		// a PersistentVolume's cost belongs to the claim, which outlives any workload
		// bound to it. Charging it to whichever pod currently holds the mount would
		// make cost jump between workloads on every rollout.
		hourly := c.table.HourlyUSD(u.CPUCores, u.MemoryGiB, 0)
		costs = append(costs, Cost{
			Usage:      *u,
			HourlyUSD:  hourly,
			MonthlyUSD: hourly * c.table.Spec.HoursPerMonth,
		})
	}

	sort.Slice(costs, func(i, j int) bool {
		if costs[i].Namespace != costs[j].Namespace {
			return costs[i].Namespace < costs[j].Namespace
		}
		if costs[i].Workload != costs[j].Workload {
			return costs[i].Workload < costs[j].Workload
		}
		return costs[i].Basis < costs[j].Basis
	})
	return costs, nil
}
