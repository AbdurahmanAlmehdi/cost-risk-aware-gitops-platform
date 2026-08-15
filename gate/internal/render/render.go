// Package render turns a Git ref into the set of Kubernetes objects that ref would
// deploy.
//
// It renders through kustomize rather than reading the YAML files directly, because M3
// (ArgoCD) renders through kustomize. If the gate parsed deployment.yaml while ArgoCD
// applied a kustomize overlay, the two would diverge the moment an overlay changed
// anything — and the LLD's central contract is that what M2 evaluates is byte-identical
// to what M3 deploys. Reading raw files would quietly break that guarantee in exactly
// the cases where it matters most.
package render

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
)

// kustomizationNames are the filenames kustomize recognises as a build root.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

type Repo struct {
	root string
}

func NewRepo(root string) (*Repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Repo{root: abs}, nil
}

func (r *Repo) Root() string { return r.root }

// ChangedFiles lists files that differ between base and head under the given pathspec.
//
// The three-dot form compares head against the merge-base of the two refs, which is what
// a pull request actually proposes. A two-dot diff would also report changes that landed
// on the base branch since the PR was opened, and the gate would block a PR for cost
// someone else introduced.
func (r *Repo) ChangedFiles(base, head, pathspec string) ([]string, error) {
	args := []string{"diff", "--name-only", fmt.Sprintf("%s...%s", base, head), "--", pathspec}
	out, err := r.git(args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}

// UncommittedChanges lists modified-but-uncommitted files under the given pathspec.
//
// The gate compares commits, which is exactly right in CI and a trap locally: running it
// against a dirty working tree silently reports "nothing to evaluate" and exits 0. That
// false green is the worst possible local behaviour — it tells an author their change is
// fine before the change exists as far as the gate is concerned.
func (r *Repo) UncommittedChanges(pathspec string) ([]string, error) {
	out, err := r.git("status", "--porcelain", "--", pathspec)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

// Worktree materialises a ref in a temporary directory so it can be rendered.
//
// A detached worktree is used rather than checking out in place: the gate must be able
// to render the base commit without disturbing the working tree it was invoked from,
// and without leaving the repository on a different ref if it exits early.
func (r *Repo) Worktree(ref string) (dir string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "gate-worktree-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	// The temp dir must not exist for `git worktree add`, which insists on creating it.
	target := filepath.Join(tmp, "tree")

	if _, err := r.git("worktree", "add", "--detach", "--quiet", target, ref); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("materialise ref %s: %w\n\n"+
			"If this runs in CI, the checkout is probably shallow. The gate needs full history "+
			"to reach the base commit — set `fetch-depth: 0` on actions/checkout.", ref, err)
	}

	cleanup = func() {
		// Prune the administrative entry as well as the directory, otherwise the repo
		// accumulates stale worktree registrations across runs.
		_, _ = r.git("worktree", "remove", "--force", target)
		os.RemoveAll(tmp)
	}
	return target, cleanup, nil
}

func (r *Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Roots maps changed files to the set of directories that must be rendered.
//
// A change to any file inside a kustomize build root can alter every object that root
// produces, so the unit of evaluation is the root, not the file. Walking up from each
// changed file — rather than rendering everything under manifests/ — keeps the gate's
// report scoped to what the pull request actually touched.
func Roots(treeDir string, changedFiles []string, manifestsPath string) ([]string, error) {
	seen := make(map[string]struct{})
	var roots []string

	manifestsAbs := filepath.Clean(filepath.Join(treeDir, manifestsPath))

	for _, f := range changedFiles {
		dir := filepath.Dir(filepath.Join(treeDir, f))

		// Walk upward looking for a kustomization, stopping at the manifests root so a
		// stray kustomization.yaml higher in the repo can never pull unrelated
		// directories into the evaluation.
		root := ""
		for cur := dir; strings.HasPrefix(cur, manifestsAbs); cur = filepath.Dir(cur) {
			if hasKustomization(cur) {
				root = cur
				break
			}
			if cur == manifestsAbs {
				break
			}
		}
		if root == "" {
			// No kustomization: the directory itself is rendered as plain YAML.
			root = dir
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}

	sort.Strings(roots)
	return roots, nil
}

// AllRoots enumerates every renderable directory under the manifests tree.
//
// Used when a change has cluster-wide cost consequences that the changed-file list cannot
// express. Editing a rate in the pricing table touches one file in one directory, but it
// re-prices every workload in the repository — evaluating only the root that happens to
// contain the table would report a trivial delta for a change that alters the entire bill.
func AllRoots(treeDir, manifestsPath string) ([]string, error) {
	manifestsAbs := filepath.Clean(filepath.Join(treeDir, manifestsPath))

	var roots []string
	err := filepath.WalkDir(manifestsAbs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if hasKustomization(path) {
			roots = append(roots, path)
			// A nested directory is a component of this build, not a root of its own;
			// descending would render the same objects twice and double their cost.
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate manifest roots under %s: %w", manifestsAbs, err)
	}

	sort.Strings(roots)
	return roots, nil
}

func hasKustomization(dir string) bool {
	for _, name := range kustomizationNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// Dir renders one directory into decoded objects.
//
// A directory that does not exist yields no objects and no error: at the base ref, a
// newly-added manifest directory is legitimately absent, and that is an "added"
// workload rather than a failure.
func Dir(dir string) ([]cost.Object, error) {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	if hasKustomization(dir) {
		return renderKustomize(dir)
	}
	return renderPlainYAML(dir)
}

func renderKustomize(dir string) ([]cost.Object, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	resMap, err := k.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	out, err := resMap.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("serialise kustomize output for %s: %w", dir, err)
	}
	return decodeDocuments(out, dir)
}

func renderPlainYAML(dir string) ([]cost.Object, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var objects []cost.Object
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		objs, err := decodeDocuments(raw, path)
		if err != nil {
			return nil, err
		}
		objects = append(objects, objs...)
	}
	return objects, nil
}

// decodeDocuments splits a multi-document YAML stream and decodes each document.
func decodeDocuments(raw []byte, source string) ([]cost.Object, error) {
	var objects []cost.Object
	for i, doc := range splitYAMLDocuments(raw) {
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		var obj cost.Object
		if err := yaml.Unmarshal(trimmed, &obj); err != nil {
			return nil, fmt.Errorf("parse document %d in %s: %w", i+1, source, err)
		}
		// Comment-only documents decode to nil rather than failing.
		if obj == nil {
			continue
		}
		if _, ok := obj["kind"]; !ok {
			return nil, fmt.Errorf("document %d in %s has no kind", i+1, source)
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

// splitYAMLDocuments splits on document separators that begin a line, so that a `---`
// appearing inside a block scalar (a shell script in a ConfigMap, say) is not mistaken
// for a separator.
func splitYAMLDocuments(raw []byte) [][]byte {
	lines := bytes.Split(raw, []byte("\n"))
	var docs [][]byte
	var current [][]byte

	for _, line := range lines {
		if isDocumentSeparator(line) {
			docs = append(docs, bytes.Join(current, []byte("\n")))
			current = nil
			continue
		}
		current = append(current, line)
	}
	docs = append(docs, bytes.Join(current, []byte("\n")))
	return docs
}

func isDocumentSeparator(line []byte) bool {
	trimmed := bytes.TrimRight(line, " \t\r")
	if !bytes.Equal(trimmed, []byte("---")) {
		return false
	}
	// A separator is only a separator at column zero; indentation means it is content.
	return !bytes.HasPrefix(line, []byte(" ")) && !bytes.HasPrefix(line, []byte("\t"))
}
