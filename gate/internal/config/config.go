// Package config loads the gate's behaviour from version-controlled YAML.
//
// Nothing that changes a verdict is compiled into the binary. A reviewer must be able to
// see, in the repository, every threshold and rule that caused their pull request to be
// blocked, and change it through the same review process as any other change (LLD §9.1).
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

type Config struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name string `json:"name"`
}

type Spec struct {
	Paths   Paths         `json:"paths"`
	Cost    CostConfig    `json:"cost"`
	Policy  PolicyConfig  `json:"policy"`
	Report  ReportConfig  `json:"report"`
	Explain ExplainConfig `json:"explain"`
}

type Paths struct {
	Manifests string `json:"manifests"`
	Policies  string `json:"policies"`
	Pricing   string `json:"pricing"`
}

// Mode selects which cost thresholds participate in the verdict.
type Mode string

const (
	ModeAbsolute Mode = "absolute"
	ModePercent  Mode = "percent"
	ModeBoth     Mode = "both"
)

type CostConfig struct {
	Mode                   Mode              `json:"mode"`
	Block                  Thresholds        `json:"block"`
	Warn                   Thresholds        `json:"warn"`
	AllowUnboundedDecrease bool              `json:"allowUnboundedDecrease"`
	AssumedNodeCount       int               `json:"assumedNodeCount"`
	Baseline               string            `json:"baseline"`
	Autoscaling            AutoscalingBudget `json:"autoscaling"`
}

// AutoscalingBudget governs elastic headroom separately from committed spend.
//
// The thresholds above apply to the floor: spend the change commits to unconditionally.
// These apply to the ceiling: spend it authorises under load. Judging both by one budget
// gets both wrong, a percentage cap tight enough to catch a careless replica bump makes
// any autoscaler unadoptable, since elasticity's whole purpose is a large ratio between
// idle and peak.
type AutoscalingBudget struct {
	// MaxCeilingDeltaUSD caps how much additional burst capacity a single change may
	// authorise, in monthly terms at full scale.
	MaxCeilingDeltaUSD float64 `json:"maxCeilingDeltaUSD"`
	// WarnCeilingDeltaUSD reports without blocking.
	WarnCeilingDeltaUSD float64 `json:"warnCeilingDeltaUSD"`
	// MaxBurstRatio caps how many times its committed cost a workload may burst to.
	// An unbounded ratio is how a workload ends up idling at $2 and capable of $2,000 -
	// each individual change looking reasonable on its own.
	MaxBurstRatio float64 `json:"maxBurstRatio"`
}

type Thresholds struct {
	MaxMonthlyDeltaUSD float64 `json:"maxMonthlyDeltaUSD"`
	MaxPercentIncrease float64 `json:"maxPercentIncrease"`
}

type PolicyConfig struct {
	Engine         string   `json:"engine"`
	FailOnSeverity []string `json:"failOnSeverity"`
	// SeverityOverrides downgrades (or upgrades) individual rules without editing the
	// rule itself. This exists so a new rule can be introduced as a warning, observed
	// against real pull requests, and promoted to blocking once the manifests it
	// governs actually comply, rather than landing as a block that fails every open
	// PR the day it merges. The override is recorded in the report, so a downgraded
	// rule is never invisible.
	SeverityOverrides    map[string]string `json:"severityOverrides,omitempty"`
	TreatErrorAsBlocking bool              `json:"treatErrorAsBlocking"`
}

// Severity returns the effective severity for a rule, applying any override.
func (p PolicyConfig) Severity(rule, declared string) (severity string, overridden bool) {
	if s, ok := p.SeverityOverrides[rule]; ok && s != declared {
		return s, true
	}
	return declared, false
}

type ReportConfig struct {
	StatusCheckName string `json:"statusCheckName"`
	CommentStrategy string `json:"commentStrategy"`
}

type ExplainConfig struct {
	Enabled        bool   `json:"enabled"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
}

// Blocks reports whether a severity should cause a failing verdict.
func (p PolicyConfig) Blocks(severity string) bool {
	for _, s := range p.FailOnSeverity {
		if s == severity {
			return true
		}
	}
	return false
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gate config: %w", err)
	}
	var cfg Config
	// UnmarshalStrict rejects unknown fields. A typo in a threshold name would
	// otherwise be silently ignored, and the gate would enforce a default the author
	// never chose, the most dangerous kind of misconfiguration, because it looks
	// like it is working.
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse gate config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid gate config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	switch c.Spec.Cost.Mode {
	case ModeAbsolute, ModePercent, ModeBoth:
	case "":
		return fmt.Errorf("spec.cost.mode is required (absolute|percent|both)")
	default:
		return fmt.Errorf("spec.cost.mode %q is not one of absolute|percent|both", c.Spec.Cost.Mode)
	}

	if c.Spec.Cost.Mode != ModePercent && c.Spec.Cost.Block.MaxMonthlyDeltaUSD <= 0 {
		return fmt.Errorf("spec.cost.block.maxMonthlyDeltaUSD must be > 0 in mode %q", c.Spec.Cost.Mode)
	}
	if c.Spec.Cost.Mode != ModeAbsolute && c.Spec.Cost.Block.MaxPercentIncrease <= 0 {
		return fmt.Errorf("spec.cost.block.maxPercentIncrease must be > 0 in mode %q", c.Spec.Cost.Mode)
	}

	// A warn threshold at or above the blocking one can never fire. It would be dead
	// configuration that reads as if warnings were enabled.
	if c.Spec.Cost.Warn.MaxMonthlyDeltaUSD >= c.Spec.Cost.Block.MaxMonthlyDeltaUSD &&
		c.Spec.Cost.Warn.MaxMonthlyDeltaUSD > 0 {
		return fmt.Errorf("spec.cost.warn.maxMonthlyDeltaUSD (%.2f) must be below block (%.2f)",
			c.Spec.Cost.Warn.MaxMonthlyDeltaUSD, c.Spec.Cost.Block.MaxMonthlyDeltaUSD)
	}
	if c.Spec.Cost.Warn.MaxPercentIncrease >= c.Spec.Cost.Block.MaxPercentIncrease &&
		c.Spec.Cost.Warn.MaxPercentIncrease > 0 {
		return fmt.Errorf("spec.cost.warn.maxPercentIncrease (%.1f) must be below block (%.1f)",
			c.Spec.Cost.Warn.MaxPercentIncrease, c.Spec.Cost.Block.MaxPercentIncrease)
	}

	if c.Spec.Cost.AssumedNodeCount < 1 {
		return fmt.Errorf("spec.cost.assumedNodeCount must be >= 1")
	}
	if c.Spec.Cost.Autoscaling.MaxCeilingDeltaUSD <= 0 {
		return fmt.Errorf("spec.cost.autoscaling.maxCeilingDeltaUSD must be > 0, otherwise a change " +
			"could authorise unlimited burst capacity without the gate noticing")
	}
	if c.Spec.Cost.Autoscaling.MaxBurstRatio < 1 {
		return fmt.Errorf("spec.cost.autoscaling.maxBurstRatio must be >= 1")
	}
	if len(c.Spec.Policy.FailOnSeverity) == 0 {
		return fmt.Errorf("spec.policy.failOnSeverity must list at least one severity")
	}
	if c.Spec.Paths.Manifests == "" || c.Spec.Paths.Policies == "" || c.Spec.Paths.Pricing == "" {
		return fmt.Errorf("spec.paths.manifests, .policies and .pricing are all required")
	}
	return nil
}
