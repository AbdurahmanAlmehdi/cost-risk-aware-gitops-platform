// Package cost turns rendered Kubernetes manifests into money.
//
// The gate prices what a manifest *requests*, not what it will use. Requests are what the
// scheduler reserves and therefore what determines how much capacity must exist; usage is
// unknowable before the workload has ever run. Where the two diverge, M4 makes the gap
// visible after deploy — the gate cannot see it in advance and does not pretend to.
package cost

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"
)

const (
	bytesPerGiB = 1024 * 1024 * 1024
)

// Resources is reserved capacity, normalised to units the pricing table understands.
type Resources struct {
	CPUCores   float64 `json:"cpu_cores"`
	MemoryGiB  float64 `json:"memory_gib"`
	StorageGiB float64 `json:"storage_gib"`
}

func (r Resources) add(o Resources) Resources {
	return Resources{r.CPUCores + o.CPUCores, r.MemoryGiB + o.MemoryGiB, r.StorageGiB + o.StorageGiB}
}

func (r Resources) scale(n float64) Resources {
	return Resources{r.CPUCores * n, r.MemoryGiB * n, r.StorageGiB * n}
}

// Workload is one priced object.
type Workload struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Replicas is the count the workload is priced at. For an autoscaled workload this
	// is the ceiling, not the current value — see MaxReplicas.
	Replicas   int       `json:"replicas"`
	PerReplica Resources `json:"per_replica"`
	Total      Resources `json:"total"`
	MonthlyUSD float64   `json:"monthly_usd"`

	// Autoscaled workloads carry their bounds so the report can show what the change
	// actually authorises rather than what it happens to be running right now.
	Autoscaled  bool `json:"autoscaled,omitempty"`
	MinReplicas int  `json:"min_replicas,omitempty"`
	MaxReplicas int  `json:"max_replicas,omitempty"`
	// FloorMonthlyUSD is the cost at the autoscaler's minimum — what this workload
	// costs when nothing is happening.
	FloorMonthlyUSD float64 `json:"floor_monthly_usd,omitempty"`
	// Flags record every assumption made while pricing this workload. They are carried
	// all the way into the pull-request comment: a number whose assumptions are hidden
	// is not auditable, and this gate blocks merges.
	Flags []string `json:"flags,omitempty"`
}

func (w Workload) Ref() string {
	if w.Namespace == "" {
		return fmt.Sprintf("%s/%s", w.Kind, w.Name)
	}
	return fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
}

// Estimate is the priced result for one manifest set.
//
// Two totals, because elastic spend and committed spend are different commitments and
// judging them by one number gets both wrong. A workload that idles at one replica and
// can burst to six commits you to the first and merely authorises the second — treating
// that as a sixfold cost increase would make autoscaling impossible to adopt, while
// treating it as no increase at all would let anyone authorise unbounded spend for free.
type Estimate struct {
	Workloads []Workload `json:"workloads"`
	// MonthlyUSD is the ceiling: what this manifest set authorises at full scale.
	MonthlyUSD float64 `json:"monthly_usd"`
	// CommittedMonthlyUSD is the floor: what it costs with nothing happening.
	CommittedMonthlyUSD float64   `json:"committed_monthly_usd"`
	Total               Resources `json:"total"`
	Flags               []string  `json:"flags,omitempty"`
}

// Calculator prices manifests against a rate table.
type Calculator struct {
	table            *pricing.Table
	assumedNodeCount int
	// Populated at the start of each Estimate call, from the object set being priced.
	autoscalers map[scaleTarget]autoscaleBounds
}

// kedaDefaultMaxReplicas is what KEDA uses when maxReplicaCount is omitted.
//
// This default is the reason the gate has to understand autoscalers at all: leaving the
// field out is a one-line manifest that authorises a hundredfold increase in spend, and
// nothing in the diff looks like a cost change.
const kedaDefaultMaxReplicas = 100

// collectAutoscalers indexes every autoscaler in the set by the workload it targets.
func (c *Calculator) collectAutoscalers(objects []Object) map[scaleTarget]autoscaleBounds {
	found := make(map[scaleTarget]autoscaleBounds)

	for _, obj := range objects {
		kind, _ := obj["kind"].(string)
		meta, _ := obj["metadata"].(map[string]any)
		namespace, _ := meta["namespace"].(string)
		spec, _ := obj["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		ref, _ := spec["scaleTargetRef"].(map[string]any)
		if ref == nil {
			continue
		}
		targetName, _ := ref["name"].(string)
		if targetName == "" {
			continue
		}
		// Both KEDA and the HPA default their target kind to Deployment when it is
		// omitted, which is the overwhelmingly common case.
		targetKind, _ := ref["kind"].(string)
		if targetKind == "" {
			targetKind = "Deployment"
		}
		target := scaleTarget{namespace: namespace, kind: targetKind, name: targetName}

		switch kind {
		case "ScaledObject":
			bounds := autoscaleBounds{
				min:    intOrDefault(spec["minReplicaCount"], 0),
				max:    intOrDefault(spec["maxReplicaCount"], kedaDefaultMaxReplicas),
				source: "KEDA ScaledObject",
			}
			if _, ok := spec["maxReplicaCount"]; !ok {
				bounds.flags = append(bounds.flags, fmt.Sprintf(
					"the ScaledObject sets no maxReplicaCount, so KEDA's default of %d applies — "+
						"this authorises up to %d replicas", kedaDefaultMaxReplicas, kedaDefaultMaxReplicas))
			}
			found[target] = bounds

		case "HorizontalPodAutoscaler":
			found[target] = autoscaleBounds{
				min:    intOrDefault(spec["minReplicas"], 1),
				max:    intOrDefault(spec["maxReplicas"], 1),
				source: "HorizontalPodAutoscaler",
			}
		}
	}
	return found
}

func New(table *pricing.Table, assumedNodeCount int) *Calculator {
	return &Calculator{table: table, assumedNodeCount: assumedNodeCount}
}

// Object is a decoded Kubernetes manifest. A plain map is used rather than typed structs
// so that an unrecognised or partially-specified manifest degrades to "not priced, and
// said so" instead of failing to decode.
type Object map[string]any

// autoscaleBounds is the replica range an autoscaler authorises for a workload.
type autoscaleBounds struct {
	min, max int
	source   string
	flags    []string
}

// scaleTarget identifies the workload an autoscaler points at.
type scaleTarget struct {
	namespace, kind, name string
}

// Estimate prices every priceable object in the set.
func (c *Calculator) Estimate(objects []Object) (Estimate, error) {
	var est Estimate

	// Autoscalers are collected first because they change how the workloads they target
	// are priced. A KEDA ScaledObject or an HPA is the single largest cost decision in a
	// repository — it authorises a ceiling — while touching no `replicas:` field at all.
	// Pricing the Deployment's literal replica count would report the cost of the
	// quietest possible moment and call it the cost of the change.
	c.autoscalers = c.collectAutoscalers(objects)

	for _, obj := range objects {
		w, priced, err := c.price(obj)
		if err != nil {
			return Estimate{}, err
		}
		if !priced {
			continue
		}
		est.Workloads = append(est.Workloads, w)
		est.Total = est.Total.add(w.Total)
		est.MonthlyUSD += w.MonthlyUSD
		est.CommittedMonthlyUSD += w.FloorMonthlyUSD
	}

	// Stable ordering so that two runs over the same manifests produce byte-identical
	// reports. A verdict that reshuffles between runs cannot be diffed or trusted.
	sort.Slice(est.Workloads, func(i, j int) bool {
		return est.Workloads[i].Ref() < est.Workloads[j].Ref()
	})
	return est, nil
}

// price returns the cost of one object. The boolean reports whether the object was
// priceable at all.
func (c *Calculator) price(obj Object) (Workload, bool, error) {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	namespace, _ := meta["namespace"].(string)

	w := Workload{Kind: kind, Name: name, Namespace: namespace}

	switch kind {
	case "Deployment", "StatefulSet", "ReplicaSet", "ReplicationController":
		spec, _ := obj["spec"].(map[string]any)
		w.Replicas = intOrDefault(spec["replicas"], 1)

		podSpec := podSpecFrom(spec)
		if podSpec == nil {
			return Workload{}, false, nil
		}
		per, flags, err := c.podResources(podSpec)
		if err != nil {
			return Workload{}, false, fmt.Errorf("%s %s/%s: %w", kind, namespace, name, err)
		}
		w.PerReplica = per
		w.Flags = append(w.Flags, flags...)

		// A StatefulSet's volumeClaimTemplates are provisioned once per replica, so
		// storage scales with replica count exactly as compute does.
		if kind == "StatefulSet" {
			storage, err := c.volumeClaimStorage(spec)
			if err != nil {
				return Workload{}, false, fmt.Errorf("%s %s/%s: %w", kind, namespace, name, err)
			}
			w.PerReplica.StorageGiB += storage
		}

	case "DaemonSet":
		spec, _ := obj["spec"].(map[string]any)
		podSpec := podSpecFrom(spec)
		if podSpec == nil {
			return Workload{}, false, nil
		}
		per, flags, err := c.podResources(podSpec)
		if err != nil {
			return Workload{}, false, fmt.Errorf("DaemonSet %s/%s: %w", namespace, name, err)
		}
		w.PerReplica = per
		w.Flags = append(w.Flags, flags...)
		// One pod per node, and the gate cannot see the cluster. The assumption is
		// stated in the flag rather than buried in the total.
		w.Replicas = c.assumedNodeCount
		w.Flags = append(w.Flags,
			fmt.Sprintf("replica count assumed to be %d (one per node); the gate does not read live cluster state", c.assumedNodeCount))

	case "Pod":
		per, flags, err := c.podResources(obj["spec"])
		if err != nil {
			return Workload{}, false, fmt.Errorf("Pod %s/%s: %w", namespace, name, err)
		}
		w.PerReplica = per
		w.Replicas = 1
		w.Flags = append(w.Flags, flags...)

	case "PersistentVolumeClaim":
		spec, _ := obj["spec"].(map[string]any)
		storage, err := c.storageRequest(spec)
		if err != nil {
			return Workload{}, false, fmt.Errorf("PVC %s/%s: %w", namespace, name, err)
		}
		w.PerReplica = Resources{StorageGiB: storage}
		w.Replicas = 1

	case "Job", "CronJob":
		// Deliberately not priced. A Job's cost is its resources multiplied by how long
		// it runs and how often it is triggered — neither is knowable from the manifest.
		// Reporting a confident figure here would be a guess wearing the same formatting
		// as the numbers that are actually derived.
		w.Replicas = 0
		w.Flags = append(w.Flags, "not priced: cost depends on runtime duration and schedule, which the manifest does not specify")
		w.Total = Resources{}
		w.MonthlyUSD = 0
		return w, true, nil

	default:
		// Services, ConfigMaps, NetworkPolicies and the rest reserve no capacity.
		return Workload{}, false, nil
	}

	// An autoscaler overrides the manifest's replica count, because once one exists the
	// number in the Deployment is only the starting point — the autoscaler owns the value
	// from then on, and M3 is configured to stop reverting it.
	//
	// The workload is priced at the CEILING. A budget has to cover what a change
	// authorises, not what it happens to be doing during review; pricing the floor would
	// let anyone raise the maximum to any number for free.
	if bounds, ok := c.autoscalers[scaleTarget{namespace: namespace, kind: kind, name: name}]; ok {
		floor := c.table.MonthlyUSD(
			w.PerReplica.scale(float64(bounds.min)).CPUCores,
			w.PerReplica.scale(float64(bounds.min)).MemoryGiB,
			w.PerReplica.scale(float64(bounds.min)).StorageGiB)

		w.Autoscaled = true
		w.MinReplicas = bounds.min
		w.MaxReplicas = bounds.max
		w.Replicas = bounds.max
		w.FloorMonthlyUSD = floor
		w.Flags = append(w.Flags, bounds.flags...)
		w.Flags = append(w.Flags, fmt.Sprintf(
			"scaled by a %s between %d and %d replicas; priced at the maximum because that is "+
				"the spend this change authorises. At the minimum it costs $%.2f/month",
			bounds.source, bounds.min, bounds.max, floor))
	}

	w.Total = w.PerReplica.scale(float64(w.Replicas))
	w.MonthlyUSD = c.table.MonthlyUSD(w.Total.CPUCores, w.Total.MemoryGiB, w.Total.StorageGiB)

	// A workload with no autoscaler is committed to its full cost — the floor and the
	// ceiling are the same number. Leaving the floor at zero here would make every
	// static workload look free in the committed total.
	if !w.Autoscaled {
		w.FloorMonthlyUSD = w.MonthlyUSD
	}
	return w, true, nil
}

// podResources computes a pod's effective resource request.
//
// This follows the Kubernetes scheduling formula rather than a naive sum, because the
// naive sum is wrong in two directions:
//
//   - Ordinary init containers run to completion before app containers start, so they
//     never hold resources at the same time. The pod's requirement is the *maximum* of
//     any single init container, not their sum.
//   - Native sidecars (init containers with restartPolicy: Always) run for the pod's
//     whole lifetime, so they DO add to the app containers' total.
//
// Getting this wrong would over- or under-charge every pod that uses init containers,
// and the error would be invisible because the number would still look plausible.
func (c *Calculator) podResources(podSpecAny any) (Resources, []string, error) {
	podSpec, ok := podSpecAny.(map[string]any)
	if !ok {
		return Resources{}, nil, nil
	}
	var flags []string

	var running Resources // app containers + native sidecars, held concurrently
	containers, _ := podSpec["containers"].([]any)
	for _, c2 := range containers {
		r, f, err := c.containerResources(c2)
		if err != nil {
			return Resources{}, nil, err
		}
		running = running.add(r)
		flags = append(flags, f...)
	}

	var maxInit Resources // ordinary init containers, held one at a time
	initContainers, _ := podSpec["initContainers"].([]any)
	for _, c2 := range initContainers {
		cm, _ := c2.(map[string]any)
		r, f, err := c.containerResources(c2)
		if err != nil {
			return Resources{}, nil, err
		}
		if restart, _ := cm["restartPolicy"].(string); restart == "Always" {
			running = running.add(r)
			flags = append(flags, f...)
			continue
		}
		maxInit = Resources{
			CPUCores:   max(maxInit.CPUCores, r.CPUCores),
			MemoryGiB:  max(maxInit.MemoryGiB, r.MemoryGiB),
			StorageGiB: max(maxInit.StorageGiB, r.StorageGiB),
		}
	}

	return Resources{
		CPUCores:   max(running.CPUCores, maxInit.CPUCores),
		MemoryGiB:  max(running.MemoryGiB, maxInit.MemoryGiB),
		StorageGiB: running.StorageGiB + maxInit.StorageGiB,
	}, flags, nil
}

func (c *Calculator) containerResources(containerAny any) (Resources, []string, error) {
	container, ok := containerAny.(map[string]any)
	if !ok {
		return Resources{}, nil, nil
	}
	name, _ := container["name"].(string)

	resources, _ := container["resources"].(map[string]any)
	requests, _ := resources["requests"].(map[string]any)

	var flags []string
	var out Resources

	if cpuRaw, ok := requests["cpu"]; ok {
		q, err := parseQuantity(cpuRaw)
		if err != nil {
			return Resources{}, nil, fmt.Errorf("container %q cpu request: %w", name, err)
		}
		// MilliValue avoids the float rounding that AsApproximateFloat64 introduces on
		// values like "100m", which would make otherwise-identical manifests produce
		// slightly different costs.
		out.CPUCores = float64(q.MilliValue()) / 1000.0
	} else {
		q, err := parseQuantity(c.table.Spec.MissingRequests.CPU)
		if err != nil {
			return Resources{}, nil, fmt.Errorf("pricing table missingRequests.cpu: %w", err)
		}
		out.CPUCores = float64(q.MilliValue()) / 1000.0
		if c.table.Spec.MissingRequests.Flag {
			flags = append(flags, fmt.Sprintf(
				"container %q declares no CPU request; priced at the assumed default %s",
				name, c.table.Spec.MissingRequests.CPU))
		}
	}

	if memRaw, ok := requests["memory"]; ok {
		q, err := parseQuantity(memRaw)
		if err != nil {
			return Resources{}, nil, fmt.Errorf("container %q memory request: %w", name, err)
		}
		out.MemoryGiB = float64(q.Value()) / bytesPerGiB
	} else {
		q, err := parseQuantity(c.table.Spec.MissingRequests.Memory)
		if err != nil {
			return Resources{}, nil, fmt.Errorf("pricing table missingRequests.memory: %w", err)
		}
		out.MemoryGiB = float64(q.Value()) / bytesPerGiB
		if c.table.Spec.MissingRequests.Flag {
			flags = append(flags, fmt.Sprintf(
				"container %q declares no memory request; priced at the assumed default %s",
				name, c.table.Spec.MissingRequests.Memory))
		}
	}

	if eph, ok := requests["ephemeral-storage"]; ok {
		q, err := parseQuantity(eph)
		if err != nil {
			return Resources{}, nil, fmt.Errorf("container %q ephemeral-storage request: %w", name, err)
		}
		out.StorageGiB = float64(q.Value()) / bytesPerGiB
	}

	return out, flags, nil
}

func (c *Calculator) volumeClaimStorage(spec map[string]any) (float64, error) {
	templates, _ := spec["volumeClaimTemplates"].([]any)
	var total float64
	for _, t := range templates {
		tm, _ := t.(map[string]any)
		tSpec, _ := tm["spec"].(map[string]any)
		s, err := c.storageRequest(tSpec)
		if err != nil {
			return 0, err
		}
		total += s
	}
	return total, nil
}

func (c *Calculator) storageRequest(spec map[string]any) (float64, error) {
	if spec == nil {
		return 0, nil
	}
	resources, _ := spec["resources"].(map[string]any)
	requests, _ := resources["requests"].(map[string]any)
	raw, ok := requests["storage"]
	if !ok {
		return 0, nil
	}
	q, err := parseQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("storage request: %w", err)
	}
	return float64(q.Value()) / bytesPerGiB, nil
}

func podSpecFrom(spec map[string]any) any {
	template, _ := spec["template"].(map[string]any)
	if template == nil {
		return nil
	}
	return template["spec"]
}

// parseQuantity accepts the several shapes a quantity takes after YAML decoding: a
// string ("500m", "128Mi"), or a bare number that YAML decoded as int or float ("cpu: 1").
func parseQuantity(v any) (resource.Quantity, error) {
	switch t := v.(type) {
	case string:
		return resource.ParseQuantity(t)
	case int:
		return *resource.NewQuantity(int64(t), resource.DecimalSI), nil
	case int64:
		return *resource.NewQuantity(t, resource.DecimalSI), nil
	case float64:
		return resource.ParseQuantity(fmt.Sprintf("%g", t))
	default:
		return resource.Quantity{}, fmt.Errorf("unsupported quantity type %T", v)
	}
}

func intOrDefault(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case nil:
		return def
	default:
		return def
	}
}
