// Command gate is M2: the pre-merge cost and policy gate.
//
// It is a pure decision engine. It reads Git, renders manifests, decides, and prints —
// it does not talk to GitHub and it holds no cluster credentials. Delivery of the verdict
// (the pull-request comment and the status check) belongs to the CI workflow, so that a
// GitHub API failure can never change a verdict, and the whole decision path can be run
// and tested offline.
//
// Exit codes are the contract with CI:
//
//	0  pass
//	1  fail          — a threshold or blocking policy fired
//	2  inconclusive  — the gate could not evaluate; blocks exactly like a failure
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/config"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/policy"
	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/render"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/report"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/verdict"
)

const (
	exitPass         = 0
	exitFail         = 1
	exitInconclusive = 2
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "evaluate" {
		fmt.Fprintln(os.Stderr, "usage: gate evaluate --config <path> --base <ref> [--head <ref>] [--format markdown|json]")
		os.Exit(exitInconclusive)
	}

	fs := flag.NewFlagSet("evaluate", flag.ExitOnError)
	var (
		configPath = fs.String("config", "gate.yaml", "path to the gate configuration")
		repoPath   = fs.String("repo", ".", "path to the repository root")
		base       = fs.String("base", "origin/main", "base ref to compare against")
		head       = fs.String("head", "HEAD", "head ref under evaluation")
		format     = fs.String("format", "markdown", "output format: markdown or json")
		outputPath = fs.String("output", "", "write the report to this file instead of stdout")
	)
	_ = fs.Parse(os.Args[2:])

	code, err := run(context.Background(), *configPath, *repoPath, *base, *head, *format, *outputPath)
	if err != nil {
		// An error here means the gate itself failed. It is reported on stderr and
		// exits inconclusive, which blocks — never passes — the pull request.
		fmt.Fprintf(os.Stderr, "gate: %v\n", err)
		os.Exit(exitInconclusive)
	}
	os.Exit(code)
}

func run(ctx context.Context, configPath, repoPath, base, head, format, outputPath string) (int, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return 0, err
	}

	repo, err := render.NewRepo(repoPath)
	if err != nil {
		return 0, err
	}

	table, err := pricing.Load(filepath.Join(repo.Root(), cfg.Spec.Paths.Pricing))
	if err != nil {
		return 0, err
	}

	changed, err := repo.ChangedFiles(base, head, cfg.Spec.Paths.Manifests)
	if err != nil {
		return 0, err
	}

	v := verdict.Verdict{
		PricingVersion: table.Metadata.Version,
		PricingSource:  table.Metadata.Source,
		Currency:       table.Spec.Currency,
		BaseRef:        base,
		HeadRef:        head,
	}

	// The comparison is between commits, so anything still sitting in the working tree
	// is invisible to it. Locally that turns an unstaged change into a confident pass.
	if dirty, dirtyErr := repo.UncommittedChanges(cfg.Spec.Paths.Manifests); dirtyErr == nil && len(dirty) > 0 {
		v.Notes = append(v.Notes, fmt.Sprintf(
			"%d uncommitted manifest change(s) were NOT evaluated — the gate compares commits. "+
				"Commit them and re-run, or this verdict does not describe your working tree.",
			len(dirty)))
	}

	// No manifest changed: there is nothing for either sub-gate to evaluate, and the
	// gate passes without pretending to have checked anything. This is reported
	// explicitly rather than as an empty green tick.
	if len(changed) == 0 {
		v.Decision = verdict.Pass
		v.Cost.Passed = true
		v.Policy.Passed = true
		v.Reasons = append(v.Reasons, "no manifest changes in this pull request; nothing to evaluate")
		return emit(v, format, outputPath)
	}

	headTree, cleanupHead, err := repo.Worktree(head)
	if err != nil {
		return 0, err
	}
	defer cleanupHead()

	baseTree, cleanupBase, err := repo.Worktree(base)
	if err != nil {
		return 0, err
	}
	defer cleanupBase()

	// Roots are resolved from the head tree: a directory that exists only at head is a
	// newly-added workload and must still be evaluated.
	roots, err := render.Roots(headTree, changed, cfg.Spec.Paths.Manifests)
	if err != nil {
		return 0, err
	}
	for _, r := range roots {
		rel, relErr := filepath.Rel(headTree, r)
		if relErr != nil {
			rel = r
		}
		v.EvaluatedRoots = append(v.EvaluatedRoots, rel)
	}

	calc := cost.New(table, cfg.Spec.Cost.AssumedNodeCount)

	var (
		headObjects []cost.Object
		baseEstimate cost.Estimate
		headEstimate cost.Estimate
		costErrors  []string
	)

	for _, root := range roots {
		rel, err := filepath.Rel(headTree, root)
		if err != nil {
			return 0, err
		}

		headObjs, err := render.Dir(root)
		if err != nil {
			// A manifest that cannot be rendered has not been shown to be safe. It is
			// recorded as an evaluation error, which blocks — rendering failures are
			// exactly the case where guessing would be most dangerous.
			costErrors = append(costErrors, fmt.Sprintf("rendering %s at head: %v", rel, err))
			continue
		}
		headObjects = append(headObjects, headObjs...)

		he, err := calc.Estimate(headObjs)
		if err != nil {
			costErrors = append(costErrors, fmt.Sprintf("pricing %s at head: %v", rel, err))
			continue
		}
		headEstimate = mergeEstimates(headEstimate, he)

		baseObjs, err := render.Dir(filepath.Join(baseTree, rel))
		if err != nil {
			costErrors = append(costErrors, fmt.Sprintf("rendering %s at base: %v", rel, err))
			continue
		}
		be, err := calc.Estimate(baseObjs)
		if err != nil {
			costErrors = append(costErrors, fmt.Sprintf("pricing %s at base: %v", rel, err))
			continue
		}
		baseEstimate = mergeEstimates(baseEstimate, be)
	}

	delta := cost.Compare(baseEstimate, headEstimate)

	// The two sub-gates are independent: a cost failure must not suppress policy
	// findings, because the author should learn about both in one round trip rather
	// than fixing cost only to be blocked again by a probe they were never told about.
	engine, err := policy.New(ctx, filepath.Join(headTree, cfg.Spec.Paths.Policies))
	var polResult policy.Result
	if err != nil {
		polResult.Errors = append(polResult.Errors, err.Error())
	} else {
		polResult = engine.Evaluate(ctx, headObjects)
	}

	aggregated := verdict.Aggregate(cfg, delta, costErrors, polResult)
	aggregated.PricingVersion = v.PricingVersion
	aggregated.PricingSource = v.PricingSource
	aggregated.Currency = v.Currency
	aggregated.BaseRef = base
	aggregated.HeadRef = head
	aggregated.EvaluatedRoots = v.EvaluatedRoots
	aggregated.Notes = v.Notes

	return emit(aggregated, format, outputPath)
}

func mergeEstimates(a, b cost.Estimate) cost.Estimate {
	a.Workloads = append(a.Workloads, b.Workloads...)
	a.MonthlyUSD += b.MonthlyUSD
	a.Flags = append(a.Flags, b.Flags...)
	a.Total.CPUCores += b.Total.CPUCores
	a.Total.MemoryGiB += b.Total.MemoryGiB
	a.Total.StorageGiB += b.Total.StorageGiB
	return a
}

func emit(v verdict.Verdict, format, outputPath string) (int, error) {
	var out string
	switch strings.ToLower(format) {
	case "markdown", "md":
		out = report.Markdown(v)
	case "json":
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return 0, fmt.Errorf("encode verdict: %w", err)
		}
		out = string(raw) + "\n"
	default:
		return 0, fmt.Errorf("unknown format %q (expected markdown or json)", format)
	}

	if outputPath == "" {
		fmt.Print(out)
	} else if err := os.WriteFile(outputPath, []byte(out), 0o644); err != nil {
		return 0, fmt.Errorf("write report: %w", err)
	}

	switch v.Decision {
	case verdict.Pass:
		return exitPass, nil
	case verdict.Fail:
		return exitFail, nil
	case verdict.Inconclusive:
		return exitInconclusive, nil
	default:
		return 0, errors.New("internal error: verdict has no decision")
	}
}
