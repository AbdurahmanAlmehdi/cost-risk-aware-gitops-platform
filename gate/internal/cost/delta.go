package cost

import "sort"

// Delta is the change in reserved spend a pull request would cause.
type Delta struct {
	BaselineMonthlyUSD  float64 `json:"baseline_monthly_usd"`
	ProjectedMonthlyUSD float64 `json:"projected_monthly_usd"`
	DeltaMonthlyUSD     float64 `json:"delta_monthly_usd"`

	// PercentIncrease is only meaningful when there is something to compare against.
	// A manifest set that did not exist before has no baseline, so "a 100% increase"
	// and "an infinite increase" are equally true and equally useless — the gate says
	// so explicitly rather than reporting a number that reads as precise.
	PercentIncrease float64 `json:"percent_increase"`
	HasPercentBasis bool    `json:"has_percent_basis"`

	// Committed* track the floor — spend the change commits to unconditionally. The
	// fields above track the ceiling, which is spend it merely authorises. The gate
	// judges them against different budgets, because they are different promises.
	CommittedBaselineUSD  float64 `json:"committed_baseline_monthly_usd"`
	CommittedProjectedUSD float64 `json:"committed_projected_monthly_usd"`
	CommittedDeltaUSD     float64 `json:"committed_delta_monthly_usd"`
	CommittedPercent      float64 `json:"committed_percent_increase"`
	HasCommittedBasis     bool    `json:"has_committed_basis"`

	Workloads []WorkloadDelta `json:"workloads"`
	Flags     []string        `json:"flags,omitempty"`
}

// WorkloadDelta is the per-workload breakdown behind the headline figure. Without it a
// reviewer is told their change costs money but not which part of it did.
type WorkloadDelta struct {
	Ref        string   `json:"ref"`
	Change     string   `json:"change"` // added | removed | modified | unchanged
	BeforeUSD  float64  `json:"before_monthly_usd"`
	AfterUSD   float64  `json:"after_monthly_usd"`
	DeltaUSD   float64  `json:"delta_monthly_usd"`
	BeforeReps int      `json:"before_replicas"`
	AfterReps  int      `json:"after_replicas"`
	Flags      []string `json:"flags,omitempty"`

	// Carried through so the report can show a range rather than a single number. A
	// reviewer reading "10" for a workload that idles at 1 would think the change was
	// far larger than it is; reading "1" for one that can reach 10 would think it far
	// smaller. Both are misleading in the direction that matters.
	Autoscaled  bool    `json:"autoscaled,omitempty"`
	MinReplicas int     `json:"min_replicas,omitempty"`
	MaxReplicas int     `json:"max_replicas,omitempty"`
	FloorUSD    float64 `json:"floor_monthly_usd,omitempty"`
}

// Compare produces the delta between a base and a head estimate.
func Compare(base, head Estimate) Delta {
	d := Delta{
		BaselineMonthlyUSD:  base.MonthlyUSD,
		ProjectedMonthlyUSD: head.MonthlyUSD,
		DeltaMonthlyUSD:     head.MonthlyUSD - base.MonthlyUSD,
	}

	if base.MonthlyUSD > 0 {
		d.PercentIncrease = (d.DeltaMonthlyUSD / base.MonthlyUSD) * 100
		d.HasPercentBasis = true
	}

	d.CommittedBaselineUSD = base.CommittedMonthlyUSD
	d.CommittedProjectedUSD = head.CommittedMonthlyUSD
	d.CommittedDeltaUSD = head.CommittedMonthlyUSD - base.CommittedMonthlyUSD
	if base.CommittedMonthlyUSD > 0 {
		d.CommittedPercent = (d.CommittedDeltaUSD / base.CommittedMonthlyUSD) * 100
		d.HasCommittedBasis = true
	}

	beforeByRef := make(map[string]Workload, len(base.Workloads))
	for _, w := range base.Workloads {
		beforeByRef[w.Ref()] = w
	}
	afterByRef := make(map[string]Workload, len(head.Workloads))
	for _, w := range head.Workloads {
		afterByRef[w.Ref()] = w
	}

	refs := make(map[string]struct{}, len(beforeByRef)+len(afterByRef))
	for ref := range beforeByRef {
		refs[ref] = struct{}{}
	}
	for ref := range afterByRef {
		refs[ref] = struct{}{}
	}

	for ref := range refs {
		before, hadBefore := beforeByRef[ref]
		after, hasAfter := afterByRef[ref]

		wd := WorkloadDelta{
			Ref:         ref,
			BeforeUSD:   before.MonthlyUSD,
			AfterUSD:    after.MonthlyUSD,
			DeltaUSD:    after.MonthlyUSD - before.MonthlyUSD,
			BeforeReps:  before.Replicas,
			AfterReps:   after.Replicas,
			Flags:       after.Flags,
			Autoscaled:  after.Autoscaled,
			MinReplicas: after.MinReplicas,
			MaxReplicas: after.MaxReplicas,
			FloorUSD:    after.FloorMonthlyUSD,
		}
		switch {
		case !hadBefore:
			wd.Change = "added"
			wd.Flags = after.Flags
		case !hasAfter:
			wd.Change = "removed"
			wd.Flags = before.Flags
		case wd.DeltaUSD != 0 || before.Replicas != after.Replicas:
			wd.Change = "modified"
		default:
			wd.Change = "unchanged"
		}
		d.Workloads = append(d.Workloads, wd)
	}

	// Largest cost increase first: the reviewer's attention should land on the line
	// that caused the verdict, not on whichever workload sorts first alphabetically.
	sort.Slice(d.Workloads, func(i, j int) bool {
		if d.Workloads[i].DeltaUSD != d.Workloads[j].DeltaUSD {
			return d.Workloads[i].DeltaUSD > d.Workloads[j].DeltaUSD
		}
		return d.Workloads[i].Ref < d.Workloads[j].Ref
	})

	d.Flags = append(d.Flags, base.Flags...)
	d.Flags = append(d.Flags, head.Flags...)
	return d
}
