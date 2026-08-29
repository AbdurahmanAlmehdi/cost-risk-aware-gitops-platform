// Package policy evaluates rendered manifests against the rules in policies/.
//
// Rego is embedded as a library rather than shelled out to conftest so the gate is a
// single binary with no runtime dependency to install, version-drift, or fail to find on
// a CI runner. The rules themselves stay ordinary Rego, reviewable by anyone who knows
// the language and testable with `opa test` independently of this code.
package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
)

// query is the contract between this package and every rule file: each rule contributes
// objects to data.gate.violation, evaluated once per manifest.
const query = "data.gate.violation"

// Violation is one rule firing against one resource.
type Violation struct {
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Remediation is optional but strongly encouraged in rules: a violation that says
	// what is wrong without saying what to do instead makes the gate an obstacle
	// rather than a guardrail.
	Remediation string `json:"remediation,omitempty"`
}

func (v Violation) Ref() string {
	if v.Namespace == "" {
		return fmt.Sprintf("%s/%s", v.Kind, v.Name)
	}
	return fmt.Sprintf("%s/%s/%s", v.Kind, v.Namespace, v.Name)
}

// Result is the outcome of evaluating a manifest set.
type Result struct {
	Violations []Violation `json:"violations"`
	// Errors are rule failures, not rule violations, a policy that could not be
	// evaluated has told us nothing about the manifest, which is a different and more
	// serious state than a policy that evaluated cleanly.
	Errors []string `json:"errors,omitempty"`
}

type Engine struct {
	prepared rego.PreparedEvalQuery
}

// New compiles every .rego file under policyDir once, up front.
//
// Compiling ahead of evaluation means a syntax error in a rule surfaces immediately as
// a gate error rather than midway through a manifest set, where it could be mistaken
// for a property of the manifest being evaluated.
func New(ctx context.Context, policyDir string) (*Engine, error) {
	files, err := regoFiles(policyDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .rego files found under %s: the policy sub-gate would pass everything, "+
			"which is indistinguishable from having no gate at all", policyDir)
	}

	r := rego.New(
		rego.Query(query),
		rego.Load(files, nil),
		// Files loaded through rego.Load are parsed as Rego v0 unless the version is
		// set explicitly, even on OPA v1.x. Without this the v1 syntax the rules are
		// written in (`if` bodies, `some x in xs`, `contains`) fails to parse.
		rego.SetRegoVersion(ast.RegoV1),
		// Strict mode rejects unused variables and other latent mistakes at compile
		// time. A rule with a typo'd variable can silently never match, and a rule
		// that never matches is a rule that never blocks anything.
		rego.Strict(true),
	)
	prepared, err := r.PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile policies in %s: %w", policyDir, err)
	}

	engine := &Engine{prepared: prepared}
	if err := engine.selfTest(ctx); err != nil {
		return nil, err
	}
	return engine, nil
}

// canary is a manifest that must always be rejected: it declares no resources, no probes
// and no security context at all.
var canary = cost.Object{
	"apiVersion": "apps/v1",
	"kind":       "Deployment",
	"metadata":   map[string]any{"name": "gate-canary", "namespace": "gate-selftest"},
	"spec": map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "gate-canary"}},
			"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "canary", "image": "example.invalid/canary:latest"},
				},
			},
		},
	},
}

// selfTest proves the rule set can still reject something before it is trusted to accept
// anything.
//
// The failure mode this exists for is silent: if the rules and the gate's input contract
// drift apart, a rule referencing a field that no longer exists, an empty policy
// directory, a package renamed. Every rule matches nothing, no error is raised, and the
// gate returns a clean pass for every pull request. A gate that approves everything is
// indistinguishable from a working gate right up to the moment it matters. Evaluating a
// manifest that must be rejected turns that silent failure into a loud one.
func (e *Engine) selfTest(ctx context.Context) error {
	result := e.Evaluate(ctx, []cost.Object{canary})
	if len(result.Errors) > 0 {
		return fmt.Errorf("policy self-test failed to evaluate: %v", result.Errors)
	}
	if len(result.Violations) == 0 {
		return fmt.Errorf("policy self-test produced no violations against a manifest that declares " +
			"no resources, no probes and no security context. The rules are not matching anything - " +
			"most likely the rule set and the gate's input contract have drifted apart, or the " +
			"policy directory is not the one you think it is. Refusing to run, because in this " +
			"state the gate would pass every pull request")
	}
	return nil
}

func regoFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".rego") && !strings.HasSuffix(path, "_test.rego") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan policy directory %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}

// Evaluate runs every rule against every object.
func (e *Engine) Evaluate(ctx context.Context, objects []cost.Object) Result {
	var result Result

	// Rules see the whole rendered set alongside the object under evaluation. Some
	// properties are only decidable across objects, whether a Service routes to a
	// Deployment, whether a workload is covered by a NetworkPolicy, and a rule that
	// can only see one manifest has to guess at them. Guessing produces false
	// positives on correct manifests, which is the fastest way to get a gate disabled.
	peers := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		peers = append(peers, map[string]any(o))
	}

	for _, obj := range objects {
		input := map[string]any{
			"manifest": map[string]any(obj),
			"peers":    peers,
		}
		rs, err := e.prepared.Eval(ctx, rego.EvalInput(input))
		if err != nil {
			// Recorded, not returned: one object failing to evaluate must not hide the
			// violations found in every other object. The caller decides what an error
			// means for the verdict.
			result.Errors = append(result.Errors,
				fmt.Sprintf("evaluating %s: %v", describe(obj), err))
			continue
		}
		if len(rs) == 0 {
			continue
		}
		values, ok := rs[0].Expressions[0].Value.([]any)
		if !ok {
			result.Errors = append(result.Errors,
				fmt.Sprintf("evaluating %s: %s returned %T, expected a set of violations",
					describe(obj), query, rs[0].Expressions[0].Value))
			continue
		}
		for _, v := range values {
			violation, err := decodeViolation(v, obj)
			if err != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("evaluating %s: %v", describe(obj), err))
				continue
			}
			result.Violations = append(result.Violations, violation)
		}
	}

	// Deterministic ordering so the same manifests always produce the same report.
	sort.Slice(result.Violations, func(i, j int) bool {
		a, b := result.Violations[i], result.Violations[j]
		if a.Ref() != b.Ref() {
			return a.Ref() < b.Ref()
		}
		return a.Rule < b.Rule
	})
	sort.Strings(result.Errors)
	return result
}

// decodeViolation converts a Rego value into a Violation, defaulting the resource
// identity from the object under evaluation so rules do not have to repeat it.
func decodeViolation(v any, obj cost.Object) (Violation, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Violation{}, fmt.Errorf("violation is %T, expected an object", v)
	}

	out := Violation{
		Rule:        stringField(m, "rule"),
		Severity:    stringField(m, "severity"),
		Message:     stringField(m, "message"),
		Remediation: stringField(m, "remediation"),
		Kind:        stringField(m, "kind"),
		Name:        stringField(m, "name"),
		Namespace:   stringField(m, "namespace"),
	}

	if out.Kind == "" {
		out.Kind, _ = obj["kind"].(string)
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if out.Name == "" {
			out.Name, _ = meta["name"].(string)
		}
		if out.Namespace == "" {
			out.Namespace, _ = meta["namespace"].(string)
		}
	}

	// A violation missing its rule ID or severity cannot be reported honestly or acted
	// on, and silently defaulting the severity could turn a blocking rule into a
	// warning. It is treated as a policy authoring error instead.
	if out.Rule == "" {
		return Violation{}, fmt.Errorf("violation has no `rule` field: %v", m)
	}
	if out.Severity == "" {
		return Violation{}, fmt.Errorf("violation %s has no `severity` field", out.Rule)
	}
	if out.Message == "" {
		return Violation{}, fmt.Errorf("violation %s has no `message` field", out.Rule)
	}
	return out, nil
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func describe(obj cost.Object) string {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if kind == "" {
		kind = "<unknown kind>"
	}
	if name == "" {
		name = "<unnamed>"
	}
	return kind + "/" + name
}
