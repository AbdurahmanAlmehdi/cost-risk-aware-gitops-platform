// Package report renders a verdict for humans.
//
// The audience is an author who has just been blocked and wants to know why, what it
// will cost, and what to change. Everything here is ordered around that: the decision
// first, the specific numbers that caused it second, the remediation third, and the
// assumptions behind the figures last but never omitted.
package report

import (
	"fmt"
	"strings"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/verdict"
)

// Marker identifies the gate's own comment so CI can update it in place rather than
// appending a new one on every push, which would bury the current verdict under stale ones.
const Marker = "<!-- cost-risk-aware-gitops-platform:gate-report -->"

// Markdown renders the pull-request comment.
func Markdown(v verdict.Verdict) string {
	var b strings.Builder

	b.WriteString(Marker)
	b.WriteString("\n## ")
	switch v.Decision {
	case verdict.Pass:
		b.WriteString("✅ Gate passed")
	case verdict.Fail:
		b.WriteString("❌ Gate failed — merge blocked")
	case verdict.Inconclusive:
		b.WriteString("⚠️ Gate inconclusive — merge blocked")
	}
	b.WriteString("\n\n")

	if v.Decision == verdict.Inconclusive {
		b.WriteString("The gate could not complete its evaluation, so it did not pass the change. " +
			"This is a fault in the gate or its inputs, not necessarily a problem with your manifests.\n\n")
	}

	// Notes come before the findings: they qualify how much the verdict below is worth,
	// and a caveat printed after the conclusion has already been read is too late.
	for _, n := range v.Notes {
		fmt.Fprintf(&b, "> ⚠️ %s\n\n", n)
	}

	if len(v.Reasons) > 0 {
		b.WriteString("**Why:**\n\n")
		for _, r := range v.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	writeCost(&b, v)
	writePolicy(&b, v)
	writeAssumptions(&b, v)
	writeProvenance(&b, v)

	return b.String()
}

func writeCost(b *strings.Builder, v verdict.Verdict) {
	d := v.Cost.Delta

	status := "passed"
	if !v.Cost.Passed {
		status = "**failed**"
	}
	fmt.Fprintf(b, "### Cost — %s\n\n", status)

	// Committed and authorised are shown side by side because they are different
	// promises. Committed is what the change costs with nothing happening; authorised is
	// the ceiling it permits under load. Collapsing them into one figure would either
	// make every autoscaler look ruinous or make unbounded burst look free.
	fmt.Fprintf(b, "| | Committed (%s/mo) | Authorised ceiling (%s/mo) |\n|---|---:|---:|\n",
		v.Currency, v.Currency)
	fmt.Fprintf(b, "| Baseline | %s | %s |\n",
		money(d.CommittedBaselineUSD), money(d.BaselineMonthlyUSD))
	fmt.Fprintf(b, "| Projected | %s | %s |\n",
		money(d.CommittedProjectedUSD), money(d.ProjectedMonthlyUSD))

	committedChange := signedMoney(d.CommittedDeltaUSD)
	if d.HasCommittedBasis {
		committedChange = fmt.Sprintf("%s (%+.1f%%)", committedChange, d.CommittedPercent)
	}
	fmt.Fprintf(b, "| **Change** | **%s** | **%s** |\n", committedChange, signedMoney(d.DeltaMonthlyUSD))
	b.WriteString("\n")

	if burst := d.DeltaMonthlyUSD - d.CommittedDeltaUSD; burst > 0 {
		fmt.Fprintf(b, "This change adds **%s/month of burst capacity** — spend it authorises "+
			"under load but does not commit to.\n\n", money(burst))
	}

	// Only workloads whose cost actually moved are listed. An unchanged workload that
	// happens to sit in a touched directory is noise in a review.
	var changed []string
	for _, w := range d.Workloads {
		if w.Change == "unchanged" {
			continue
		}
		replicas := fmt.Sprintf("%d → %d", w.BeforeReps, w.AfterReps)
		if w.BeforeReps == w.AfterReps {
			replicas = fmt.Sprintf("%d", w.AfterReps)
		}
		after := money(w.AfterUSD)
		if w.Autoscaled {
			// The ceiling is what the change authorises and therefore what is priced,
			// but showing it alone would read as the running cost. The range is the
			// honest presentation.
			replicas = fmt.Sprintf("%d–%d (auto)", w.MinReplicas, w.MaxReplicas)
			after = fmt.Sprintf("%s–**%s**", money(w.FloorUSD), money(w.AfterUSD))
		}
		changed = append(changed, fmt.Sprintf("| `%s` | %s | %s | %s | %s | **%s** |",
			w.Ref, w.Change, replicas, money(w.BeforeUSD), after, signedMoney(w.DeltaUSD)))
	}
	if len(changed) > 0 {
		b.WriteString("| Workload | Change | Replicas | Before | After | Delta |\n")
		b.WriteString("|---|---|---:|---:|---:|---:|\n")
		b.WriteString(strings.Join(changed, "\n"))
		b.WriteString("\n\n")
	}

	for _, w := range v.Cost.Warnings {
		fmt.Fprintf(b, "> ⚠️ %s\n\n", w)
	}
}

func writePolicy(b *strings.Builder, v verdict.Verdict) {
	status := "passed"
	if !v.Policy.Passed {
		status = "**failed**"
	}
	fmt.Fprintf(b, "### Policy — %s\n\n", status)

	if len(v.Policy.Blocking) == 0 && len(v.Policy.Warnings) == 0 && len(v.Policy.Errors) == 0 {
		b.WriteString("No violations.\n\n")
		return
	}

	if len(v.Policy.Blocking) > 0 {
		b.WriteString("**Blocking violations**\n\n")
		for _, viol := range v.Policy.Blocking {
			fmt.Fprintf(b, "- **`%s`** on `%s`\n  - %s\n", viol.Rule, viol.Ref(), viol.Message)
			if viol.Remediation != "" {
				fmt.Fprintf(b, "  - *Fix:* %s\n", viol.Remediation)
			}
		}
		b.WriteString("\n")
	}

	if len(v.Policy.Warnings) > 0 {
		b.WriteString("<details><summary>Warnings (not blocking)</summary>\n\n")
		for _, viol := range v.Policy.Warnings {
			fmt.Fprintf(b, "- `%s` on `%s` — %s\n", viol.Rule, viol.Ref(), viol.Message)
		}
		b.WriteString("\n</details>\n\n")
	}

	if len(v.Policy.Errors) > 0 {
		b.WriteString("**Evaluation errors**\n\n")
		for _, e := range v.Policy.Errors {
			fmt.Fprintf(b, "- %s\n", e)
		}
		b.WriteString("\n")
	}
}

// writeAssumptions surfaces every assumption the estimate rests on.
//
// These are not hidden behind a details block by accident of formatting — they are the
// difference between a figure a reviewer can challenge and a figure they must simply
// believe. A gate that blocks merges owes the author its reasoning.
func writeAssumptions(b *strings.Builder, v verdict.Verdict) {
	flags := dedupe(collectFlags(v))
	if len(flags) == 0 {
		return
	}
	b.WriteString("<details><summary>Assumptions behind these figures</summary>\n\n")
	for _, f := range flags {
		fmt.Fprintf(b, "- %s\n", f)
	}
	b.WriteString("\n</details>\n\n")
}

func collectFlags(v verdict.Verdict) []string {
	flags := append([]string{}, v.Cost.Delta.Flags...)
	for _, w := range v.Cost.Delta.Workloads {
		for _, f := range w.Flags {
			flags = append(flags, fmt.Sprintf("`%s`: %s", w.Ref, f))
		}
	}
	return flags
}

func writeProvenance(b *strings.Builder, v verdict.Verdict) {
	b.WriteString("---\n\n")
	fmt.Fprintf(b, "Cost is modelled from reserved **requests**, priced with pricing table v%d (%s). ",
		v.PricingVersion, v.PricingSource)
	b.WriteString("The cluster is local, so no money is actually spent — these figures model what the " +
		"same workload would cost on a cloud provider, and M4 measures live consumption against the " +
		"same rates after deploy.\n\n")

	if len(v.EvaluatedRoots) > 0 {
		b.WriteString("<details><summary>Evaluated manifest roots</summary>\n\n")
		for _, r := range v.EvaluatedRoots {
			fmt.Fprintf(b, "- `%s`\n", r)
		}
		b.WriteString("\n</details>\n")
	}
}

func money(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func signedMoney(v float64) string {
	if v >= 0 {
		return fmt.Sprintf("+$%.2f", v)
	}
	return fmt.Sprintf("−$%.2f", -v)
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
