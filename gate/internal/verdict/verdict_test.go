package verdict_test

import (
	"testing"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/config"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/policy"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/verdict"
)

func cfg(mutate func(*config.Config)) *config.Config {
	c := &config.Config{Spec: config.Spec{
		Cost: config.CostConfig{
			Mode:                   config.ModeBoth,
			Block:                  config.Thresholds{MaxMonthlyDeltaUSD: 25, MaxPercentIncrease: 20},
			Warn:                   config.Thresholds{MaxMonthlyDeltaUSD: 10, MaxPercentIncrease: 10},
			AllowUnboundedDecrease: true,
			AssumedNodeCount:       3,
		},
		Policy: config.PolicyConfig{
			FailOnSeverity:       []string{"block"},
			TreatErrorAsBlocking: true,
		},
	}}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func delta(deltaUSD, baseline float64) cost.Delta {
	d := cost.Delta{
		BaselineMonthlyUSD:  baseline,
		ProjectedMonthlyUSD: baseline + deltaUSD,
		DeltaMonthlyUSD:     deltaUSD,
	}
	if baseline > 0 {
		d.PercentIncrease = deltaUSD / baseline * 100
		d.HasPercentBasis = true
	}
	return d
}

// TestAbsoluteThresholdBoundary is the LLD §2.6 boundary case. The comparison is strictly
// greater-than, so a change landing exactly on the limit passes: a threshold described as
// "no more than $25" must permit $25, or the documented limit and the enforced limit
// differ by one cent and nobody can tell which is real.
func TestAbsoluteThresholdBoundary(t *testing.T) {
	cases := []struct {
		name     string
		deltaUSD float64
		want     verdict.Decision
	}{
		{"just under", 24.99, verdict.Pass},
		{"exactly on the limit", 25.00, verdict.Pass},
		{"just over", 25.01, verdict.Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A large baseline keeps the percentage well clear, isolating the absolute rule.
			v := verdict.Aggregate(cfg(nil), delta(tc.deltaUSD, 10_000), nil, policy.Result{})
			if v.Decision != tc.want {
				t.Errorf("decision = %q, want %q (reasons: %v)", v.Decision, tc.want, v.Reasons)
			}
		})
	}
}

func TestPercentThresholdBoundary(t *testing.T) {
	cases := []struct {
		name    string
		percent float64
		want    verdict.Decision
	}{
		{"just under", 19.9, verdict.Pass},
		{"exactly on the limit", 20.0, verdict.Pass},
		{"just over", 20.1, verdict.Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := 100.0
			// Keep the absolute delta small so only the percentage rule can fire.
			c := cfg(func(c *config.Config) { c.Spec.Cost.Block.MaxMonthlyDeltaUSD = 10_000 })
			v := verdict.Aggregate(c, delta(baseline*tc.percent/100, baseline), nil, policy.Result{})
			if v.Decision != tc.want {
				t.Errorf("decision = %q, want %q (reasons: %v)", v.Decision, tc.want, v.Reasons)
			}
		})
	}
}

// TestCostReductionIsNeverBlocked: a change that saves money must not be blocked for the
// size of the saving. Without the sign check, a large decrease could trip a percentage
// rule written with only increases in mind.
func TestCostReductionIsNeverBlocked(t *testing.T) {
	v := verdict.Aggregate(cfg(nil), delta(-500, 600), nil, policy.Result{})
	if v.Decision != verdict.Pass {
		t.Errorf("a cost reduction was blocked: %v", v.Reasons)
	}
}

// TestNewWorkloadFallsBackToAbsolute: with no baseline the percentage is undefined. In
// percent-only mode the check must not simply be skipped, or an unbounded new workload
// would sail through the cost gate untested.
func TestNewWorkloadFallsBackToAbsolute(t *testing.T) {
	c := cfg(func(c *config.Config) { c.Spec.Cost.Mode = config.ModePercent })
	v := verdict.Aggregate(c, delta(500, 0), nil, policy.Result{})
	if v.Decision != verdict.Fail {
		t.Errorf("a new workload adding $500/month passed in percent-only mode: %v", v.Reasons)
	}
}

// TestSubGateErrorIsInconclusiveNotPass is the fail-safe property from LLD §2.5. The
// distinction from Fail matters: "too expensive" and "the gate is broken" send the author
// to two completely different places.
func TestSubGateErrorIsInconclusiveNotPass(t *testing.T) {
	t.Run("cost error", func(t *testing.T) {
		v := verdict.Aggregate(cfg(nil), delta(1, 1000), []string{"could not render manifest"}, policy.Result{})
		if v.Decision != verdict.Inconclusive {
			t.Errorf("decision = %q, want inconclusive", v.Decision)
		}
		if !v.Decision.Blocks() {
			t.Error("an inconclusive verdict did not block the merge")
		}
	})

	t.Run("policy error", func(t *testing.T) {
		v := verdict.Aggregate(cfg(nil), delta(1, 1000), nil,
			policy.Result{Errors: []string{"rule failed to evaluate"}})
		if v.Decision != verdict.Inconclusive {
			t.Errorf("decision = %q, want inconclusive", v.Decision)
		}
	})
}

func TestBlockingViolationFailsAndWarningDoesNot(t *testing.T) {
	blocking := policy.Result{Violations: []policy.Violation{
		{Rule: "GATE-005", Severity: "block", Message: "no liveness probe", Kind: "Deployment", Name: "app"},
	}}
	if v := verdict.Aggregate(cfg(nil), delta(1, 1000), nil, blocking); v.Decision != verdict.Fail {
		t.Errorf("decision = %q, want fail", v.Decision)
	}

	warning := policy.Result{Violations: []policy.Violation{
		{Rule: "GATE-012", Severity: "warn", Message: "writable root filesystem", Kind: "Deployment", Name: "app"},
	}}
	v := verdict.Aggregate(cfg(nil), delta(1, 1000), nil, warning)
	if v.Decision != verdict.Pass {
		t.Errorf("a warning blocked the merge: %v", v.Reasons)
	}
	if len(v.Policy.Warnings) != 1 {
		t.Error("the warning was not reported")
	}
}

// TestSeverityOverrideIsApplied covers the graduated-rollout path, and asserts the
// downgrade is visible in the message — an override that hides itself would let a rule be
// silently disabled in config while still appearing to be enforced.
func TestSeverityOverrideIsApplied(t *testing.T) {
	c := cfg(func(c *config.Config) {
		c.Spec.Policy.SeverityOverrides = map[string]string{"GATE-014": "warn"}
	})
	result := policy.Result{Violations: []policy.Violation{
		{Rule: "GATE-014", Severity: "block", Message: "image not pinned", Kind: "Deployment", Name: "app"},
	}}

	v := verdict.Aggregate(c, delta(1, 1000), nil, result)
	if v.Decision != verdict.Pass {
		t.Errorf("overridden rule still blocked: %v", v.Reasons)
	}
	if len(v.Policy.Warnings) != 1 {
		t.Fatal("overridden violation was not reported as a warning")
	}
	if msg := v.Policy.Warnings[0].Message; msg == "image not pinned" {
		t.Error("the severity override is not visible in the message; a rule could be " +
			"disabled in config while still appearing to be enforced")
	}
}
