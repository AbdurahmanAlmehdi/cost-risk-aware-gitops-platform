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
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/render"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/report"
	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/verdict"
	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"
)

const (
	exitPass         = 0
	exitFail         = 1
	exitInconclusive = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "evaluate":
		evaluateCmd()
	case "price":
		priceCmd()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  gate evaluate --config <path> --base <ref> [--head <ref>] [--format markdown|json]")
	fmt.Fprintln(os.Stderr, "  gate price    --config <path> [--path <manifest dir>] [--format table|json]")
	os.Exit(exitInconclusive)
}

// priceCmd prices the manifests as they stand, with no comparison.
//
// `evaluate` answers "what does this change cost", which is the merge decision. This
// answers "what does this cost", which is the figure M4 measures against: it is what makes
// the platform's central claim checkable rather than merely asserted, since the same rate
// table and the same code produce the pre-merge estimate and the live attribution.
func priceCmd() {
	fs := flag.NewFlagSet("price", flag.ExitOnError)
	var (
		configPath = fs.String("config", "gate.yaml", "path to the gate configuration")
		repoPath   = fs.String("repo", ".", "path to the repository root")
		path       = fs.String("path", "", "manifest directory to price (default: every root)")
		format     = fs.String("format", "table", "output format: table or json")
	)
	_ = fs.Parse(os.Args[2:])

	if err := runPrice(*configPath, *repoPath, *path, *format); err != nil {
		fmt.Fprintf(os.Stderr, "gate: %v\n", err)
		os.Exit(exitInconclusive)
	}
}

func runPrice(configPath, repoPath, path, format string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	repo, err := render.NewRepo(repoPath)
	if err != nil {
		return err
	}
	table, err := pricing.Load(filepath.Join(repo.Root(), cfg.Spec.Paths.Pricing))
	if err != nil {
		return err
	}

	var roots []string
	if path != "" {
		roots = []string{filepath.Join(repo.Root(), path)}
	} else {
		if roots, err = render.AllRoots(repo.Root(), cfg.Spec.Paths.Manifests); err != nil {
			return err
		}
	}

	calc := cost.New(table, cfg.Spec.Cost.AssumedNodeCount)
	var estimate cost.Estimate
	for _, root := range roots {
		objects, err := render.Dir(root)
		if err != nil {
			return fmt.Errorf("rendering %s: %w", root, err)
		}
		est, err := calc.Estimate(objects)
		if err != nil {
			return fmt.Errorf("pricing %s: %w", root, err)
		}
		estimate = mergeEstimates(estimate, est)
	}

	if strings.ToLower(format) == "json" {
		raw, err := json.MarshalIndent(map[string]any{
			"pricing_version": table.Metadata.Version,
			"currency":        table.Spec.Currency,
			"monthly_usd":     estimate.MonthlyUSD,
			"workloads":       estimate.Workloads,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}

	fmt.Printf("%-40s %10s %14s\n", "WORKLOAD", "REPLICAS", "MONTHLY "+table.Spec.Currency)
	for _, w := range estimate.Workloads {
		fmt.Printf("%-40s %10d %14.2f\n", w.Ref(), w.Replicas, w.MonthlyUSD)
	}
	fmt.Printf("%-40s %10s %14.2f\n", "TOTAL", "", estimate.MonthlyUSD)
	return nil
}

func evaluateCmd() {
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

	changed, err := repo.ChangedFiles(base, head, cfg.Spec.Paths.Manifests)
	if err != nil {
		return 0, err
	}

	// Provenance is filled in from the head table once the worktrees exist; this copy
	// only serves the early-return paths below.
	table, err := pricing.Load(filepath.Join(repo.Root(), cfg.Spec.Paths.Pricing))
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

	// Each side is priced with its own rate table, so a change to the rates shows up as a
	// real cost delta. Pricing both sides from one table would make a repricing of the
	// entire estate report as $0.00 — the two figures would differ only by whatever else
	// the pull request happened to touch.
	baseTable, headTable, pricingNote, err := loadPricingTables(baseTree, headTree, cfg.Spec.Paths.Pricing)
	if err != nil {
		return 0, err
	}
	v.PricingVersion = headTable.Metadata.Version
	v.PricingSource = headTable.Metadata.Source
	v.Currency = headTable.Spec.Currency
	if pricingNote != "" {
		v.Notes = append(v.Notes, pricingNote)
	}

	// Roots are resolved from the head tree: a directory that exists only at head is a
	// newly-added workload and must still be evaluated.
	roots, err := render.Roots(headTree, changed, cfg.Spec.Paths.Manifests)
	if err != nil {
		return 0, err
	}

	// A rate change is a one-file diff with cluster-wide consequences. Scoping the
	// evaluation to the directory that happens to contain the pricing table would report
	// a trivial delta for a change that re-prices everything the platform runs.
	if ratesChanged(baseTable, headTable) {
		allRoots, rootsErr := render.AllRoots(headTree, cfg.Spec.Paths.Manifests)
		if rootsErr != nil {
			return 0, rootsErr
		}
		roots = allRoots
		v.Notes = append(v.Notes, fmt.Sprintf(
			"the pricing table changed (v%d → v%d), so **every** manifest root was re-priced, "+
				"not only the ones this pull request edits — a rate change alters the cost of "+
				"everything the platform runs.",
			baseTable.Metadata.Version, headTable.Metadata.Version))
	}
	for _, r := range roots {
		rel, relErr := filepath.Rel(headTree, r)
		if relErr != nil {
			rel = r
		}
		v.EvaluatedRoots = append(v.EvaluatedRoots, rel)
	}

	baseCalc := cost.New(baseTable, cfg.Spec.Cost.AssumedNodeCount)
	headCalc := cost.New(headTable, cfg.Spec.Cost.AssumedNodeCount)

	var (
		headObjects  []cost.Object
		baseEstimate cost.Estimate
		headEstimate cost.Estimate
		costErrors   []string
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

		he, err := headCalc.Estimate(headObjs)
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
		be, err := baseCalc.Estimate(baseObjs)
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

// loadPricingTables reads the rate table as it exists on each side of the comparison.
//
// The base table may legitimately be absent — the pull request that first introduces a
// pricing table has no earlier version to compare against. In that case the head table
// stands in for both sides and the substitution is reported, because silently pricing the
// baseline with new rates would make an unrelated repricing look like a free change.
func loadPricingTables(baseTree, headTree, pricingPath string) (base, head *pricing.Table, note string, err error) {
	head, err = pricing.Load(filepath.Join(headTree, pricingPath))
	if err != nil {
		return nil, nil, "", fmt.Errorf("pricing table at head: %w", err)
	}

	base, err = pricing.Load(filepath.Join(baseTree, pricingPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return head, head, "no pricing table exists at the base commit, so the baseline was " +
				"priced with the incoming rates; the delta reflects manifest changes only", nil
		}
		return nil, nil, "", fmt.Errorf("pricing table at base: %w", err)
	}
	return base, head, "", nil
}

// ratesChanged reports whether the two tables would price identical manifests differently.
//
// Only fields that affect a computed figure are compared. Editing the table's `source`
// annotation or its retrieval date changes the document without changing a single price,
// and re-pricing the entire repository for a comment edit would bury the real signal.
func ratesChanged(base, head *pricing.Table) bool {
	return base.Spec.Rates != head.Spec.Rates ||
		base.Spec.HoursPerMonth != head.Spec.HoursPerMonth ||
		base.Spec.MissingRequests != head.Spec.MissingRequests ||
		base.Spec.Currency != head.Spec.Currency
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
