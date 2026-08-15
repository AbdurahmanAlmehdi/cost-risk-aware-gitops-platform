// Package verdict combines the cost and policy sub-gates into one merge decision.
//
// The aggregation rule is deliberately simple and stated in one place: a pull request
// passes if and only if both sub-gates pass. Anything that could not be evaluated is
// treated as a failure, never as a pass — the gate is the last thing standing between a
// change and production, and "we could not tell" must never open it (LLD §2.5, §9.2).
package verdict

import (
	"fmt"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/config"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/policy"
)

type Decision string

const (
	Pass Decision = "pass"
	Fail Decision = "fail"
	// Inconclusive means a sub-gate errored rather than returning a clean answer. It
	// blocks exactly like Fail, but is reported distinctly: "your change is too
	// expensive" and "the gate is broken" call for completely different responses from
	// the author, and collapsing them would send people to debug the wrong thing.
	Inconclusive Decision = "inconclusive"
)

// Blocks reports whether this decision prevents a merge.
func (d Decision) Blocks() bool { return d != Pass }

type Verdict struct {
	Decision Decision `json:"decision"`
	// Reasons are the specific findings that produced a blocking decision, in the
	// order they should be read.
	Reasons []string `json:"reasons,omitempty"`
	// Notes are caveats about the evaluation itself rather than findings about the
	// change — conditions the reader needs in order to know how much the verdict is
	// worth.
	Notes []string `json:"notes,omitempty"`

	Cost   CostVerdict   `json:"cost"`
	Policy PolicyVerdict `json:"policy"`

	// Provenance: which config and rate table produced this verdict. A blocked pull
	// request must be explainable months later, when both may have changed.
	PricingVersion int    `json:"pricing_version"`
	PricingSource  string `json:"pricing_source"`
	Currency       string `json:"currency"`
	BaseRef        string `json:"base_ref"`
	HeadRef        string `json:"head_ref"`
	EvaluatedRoots []string `json:"evaluated_roots"`
}

type CostVerdict struct {
	Delta cost.Delta `json:"delta"`
	// Passed is false when a blocking threshold fired.
	Passed bool `json:"passed"`
	// Warned is true when a warn threshold fired without blocking. A warning is
	// reported and deliberately does not affect the decision.
	Warned bool `json:"warned"`
	// Fired names every threshold that was crossed, so the comment can say which
	// number caused the block rather than leaving the author to infer it.
	Fired    []string `json:"fired,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

type PolicyVerdict struct {
	Blocking []policy.Violation `json:"blocking,omitempty"`
	Warnings []policy.Violation `json:"warnings,omitempty"`
	Errors   []string           `json:"errors,omitempty"`
	Passed   bool               `json:"passed"`
}

// Aggregate combines both sub-gate results under the supplied configuration.
func Aggregate(cfg *config.Config, delta cost.Delta, costErrors []string, pol policy.Result) Verdict {
	v := Verdict{
		Cost:   evaluateCost(cfg.Spec.Cost, delta, costErrors),
		Policy: evaluatePolicy(cfg.Spec.Policy, pol),
	}

	// Errors first: an inconclusive result is a different kind of problem from a
	// failing one and should be the headline when it occurs.
	inconclusive := len(v.Cost.Errors) > 0 ||
		(cfg.Spec.Policy.TreatErrorAsBlocking && len(v.Policy.Errors) > 0)

	switch {
	case inconclusive:
		v.Decision = Inconclusive
	case !v.Cost.Passed || !v.Policy.Passed:
		v.Decision = Fail
	default:
		v.Decision = Pass
	}

	// Errors are summarised here and printed in full in their own section. A compile
	// failure can run to dozens of lines, and repeating it verbatim in the summary
	// pushes the cost and policy findings off the top of the comment.
	if len(v.Cost.Errors) > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the cost sub-gate could not be evaluated (%d error(s)); see the Cost section below",
			len(v.Cost.Errors)))
	}
	if cfg.Spec.Policy.TreatErrorAsBlocking && len(v.Policy.Errors) > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the policy sub-gate could not be evaluated (%d error(s)); see the Policy section below",
			len(v.Policy.Errors)))
	}
	v.Reasons = append(v.Reasons, v.Cost.Fired...)
	for _, viol := range v.Policy.Blocking {
		v.Reasons = append(v.Reasons, fmt.Sprintf("%s on %s: %s", viol.Rule, viol.Ref(), viol.Message))
	}
	return v
}

func evaluateCost(cfg config.CostConfig, delta cost.Delta, errors []string) CostVerdict {
	cv := CostVerdict{Delta: delta, Passed: true, Errors: errors}
	if len(errors) > 0 {
		cv.Passed = false
		return cv
	}

	// A change that reduces spend is never blocked for being too large a reduction.
	if delta.DeltaMonthlyUSD <= 0 && cfg.AllowUnboundedDecrease {
		return cv
	}

	checkAbsolute := cfg.Mode == config.ModeAbsolute || cfg.Mode == config.ModeBoth
	checkPercent := cfg.Mode == config.ModePercent || cfg.Mode == config.ModeBoth

	// Percentage is undefined against a zero baseline. Rather than skip the check —
	// which would let an unlimited new workload through in percent mode — the absolute
	// threshold stands in, and the report says that is what happened.
	if checkPercent && !delta.HasPercentBasis {
		checkPercent = false
		if !checkAbsolute {
			checkAbsolute = true
			cv.Warnings = append(cv.Warnings,
				"no cost baseline exists for this change (the workload is new), so a percentage "+
					"increase is undefined; the absolute threshold was applied instead")
		}
	}

	if checkAbsolute && delta.DeltaMonthlyUSD > cfg.Block.MaxMonthlyDeltaUSD {
		cv.Passed = false
		cv.Fired = append(cv.Fired, fmt.Sprintf(
			"projected monthly cost increases by $%.2f, above the $%.2f limit for a single change",
			delta.DeltaMonthlyUSD, cfg.Block.MaxMonthlyDeltaUSD))
	} else if checkAbsolute && cfg.Warn.MaxMonthlyDeltaUSD > 0 &&
		delta.DeltaMonthlyUSD > cfg.Warn.MaxMonthlyDeltaUSD {
		cv.Warned = true
		cv.Warnings = append(cv.Warnings, fmt.Sprintf(
			"projected monthly cost increases by $%.2f, above the $%.2f advisory level",
			delta.DeltaMonthlyUSD, cfg.Warn.MaxMonthlyDeltaUSD))
	}

	if checkPercent && delta.PercentIncrease > cfg.Block.MaxPercentIncrease {
		cv.Passed = false
		cv.Fired = append(cv.Fired, fmt.Sprintf(
			"projected monthly cost increases by %.1f%%, above the %.1f%% limit for a single change",
			delta.PercentIncrease, cfg.Block.MaxPercentIncrease))
	} else if checkPercent && cfg.Warn.MaxPercentIncrease > 0 &&
		delta.PercentIncrease > cfg.Warn.MaxPercentIncrease {
		cv.Warned = true
		cv.Warnings = append(cv.Warnings, fmt.Sprintf(
			"projected monthly cost increases by %.1f%%, above the %.1f%% advisory level",
			delta.PercentIncrease, cfg.Warn.MaxPercentIncrease))
	}

	return cv
}

func evaluatePolicy(cfg config.PolicyConfig, result policy.Result) PolicyVerdict {
	pv := PolicyVerdict{Errors: result.Errors, Passed: true}
	for _, v := range result.Violations {
		if severity, overridden := cfg.Severity(v.Rule, v.Severity); overridden {
			// Recorded in the message so a downgraded rule is visible in the report
			// rather than silently absent from the blocking list.
			v.Message = fmt.Sprintf("%s _(severity overridden: %s → %s in gate.yaml)_",
				v.Message, v.Severity, severity)
			v.Severity = severity
		}
		if cfg.Blocks(v.Severity) {
			pv.Blocking = append(pv.Blocking, v)
			pv.Passed = false
			continue
		}
		pv.Warnings = append(pv.Warnings, v)
	}
	if cfg.TreatErrorAsBlocking && len(result.Errors) > 0 {
		pv.Passed = false
	}
	return pv
}
