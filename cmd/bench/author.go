package main

// prep-author mines greenfield commits structurally: no subject regex,
// the shape is the classifier (docs/specs/authoring-bench.md, Mining).
// Every gate that rejects names its blocker; a silent skip hides the
// roster gap the spec calls out.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/guygrigsby/agent-go/bench"
)

type (
	AuthorSpec = bench.AuthorSpec
	AuthorFile = bench.AuthorFile
)

// authorCoverageTimeout bounds the `go test -cover` run in the coverage
// gate: mined tasks are echo-server scale (docs/specs/authoring-bench.md,
// Mining), so a survivor that cannot finish in this window is not the
// small task the spec asks for.
const authorCoverageTimeout = 2 * time.Minute

type authorShape struct {
	Dir   string
	Tests []string
	Impls []string
}

// authorShapeOf classifies one commit's changed paths: all Go files in a
// single directory, at least one test and one implementation, no module
// file edits. Non-Go passengers (README, configs) ride along ignored.
func authorShapeOf(paths []string) (authorShape, bool) {
	var s authorShape
	for _, p := range paths {
		if p == "" {
			continue
		}
		base := filepath.Base(p)
		if base == "go.mod" || base == "go.sum" {
			return authorShape{}, false
		}
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		dir := filepath.Dir(p)
		if s.Dir == "" {
			s.Dir = dir
		} else if s.Dir != dir {
			return authorShape{}, false
		}
		if strings.HasSuffix(p, "_test.go") {
			s.Tests = append(s.Tests, p)
		} else {
			s.Impls = append(s.Impls, p)
		}
	}
	if s.Dir == "" || len(s.Tests) == 0 || len(s.Impls) == 0 {
		return authorShape{}, false
	}
	return s, true
}

var coverageRe = regexp.MustCompile(`coverage: (\d+(?:\.\d+)?)% of statements`)

func parseCoverage(out string) (float64, bool) {
	m := coverageRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func authorPrompt(pkg, dir string, tests []string) string {
	return fmt.Sprintf("Author the Go package %s. Its tests are already in place (%s); implement the package in %s so they pass. Do not modify the test files.",
		pkg, strings.Join(tests, ", "), dir)
}

// prepAuthor walks every repo clone under scratch, mining greenfield
// commits structurally and writing the survivors as author manifests.
func prepAuthor(scratch, outFile string, minCover float64, maxImplFiles, maxImplLines int) error {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return err
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoName := e.Name()
		out = append(out, authorCandidates(path.Join(scratch, repoName), repoName, minCover, maxImplFiles, maxImplLines)...)
	}
	b, _ := json.MarshalIndent(out, "", " ")
	if err := os.WriteFile(outFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d candidates -> %s\n", len(out), outFile)
	return nil
}

// authorCandidates scans one clone's full history (structural scan, no
// subject regex) and returns the survivors.
func authorCandidates(repoDir, repoName string, minCover float64, maxImplFiles, maxImplLines int) []Manifest {
	out, err := exec.Command("git", "-C", repoDir, "log", "--no-merges", "--format=%H%x00%s").Output()
	if err != nil {
		return nil
	}
	var manifests []Manifest
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		sha, _, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		if m, ok := authorCandidate(repoDir, repoName, sha, minCover, maxImplFiles, maxImplLines); ok {
			manifests = append(manifests, m)
		}
	}
	return manifests
}

// authorCandidate runs one commit through the gate ladder: shape, tier 1
// ceilings (impl file count, impl line count, dir must be new, module
// path must derive), then the coverage gate last since it is the
// expensive one (a temp worktree, a real `go test -cover`). Every
// rejection past the shape check logs its blocker to stderr; the shape
// mismatch itself stays silent, it is the common case (most commits are
// not greenfield package additions).
func authorCandidate(repoDir, repoName, sha string, minCover float64, maxImplFiles, maxImplLines int) (Manifest, bool) {
	skip := func(why string) (Manifest, bool) {
		fmt.Fprintf(os.Stderr, "skip %s %.8s: %s\n", repoName, sha, why)
		return Manifest{}, false
	}

	shape, ok := authorShapeOf(diffTreeFiles(repoDir, sha))
	if !ok {
		return Manifest{}, false
	}
	if len(shape.Impls) > maxImplFiles {
		return skip(fmt.Sprintf("%d impl files over max %d", len(shape.Impls), maxImplFiles))
	}
	implLines := 0
	for _, f := range shape.Impls {
		implLines += countLines(gitShow(repoDir, sha+":"+f))
	}
	if implLines > maxImplLines {
		return skip(fmt.Sprintf("%d impl lines over max %d", implLines, maxImplLines))
	}
	if dirExistsAt(repoDir, sha+"^", shape.Dir) {
		return skip(fmt.Sprintf("dir %s existed at parent", shape.Dir))
	}
	mod, ok := modulePathAt(repoDir, sha)
	if !ok {
		return skip("module path underivable from go.mod")
	}
	pkg := mod
	if shape.Dir != "." {
		pkg = mod + "/" + shape.Dir
	}

	testFiles := make([]AuthorFile, 0, len(shape.Tests))
	for _, f := range shape.Tests {
		content := gitShow(repoDir, sha+":"+f)
		if content == "" {
			return skip(fmt.Sprintf("test file %s unreadable at %.8s", f, sha))
		}
		testFiles = append(testFiles, AuthorFile{Path: f, Content: content})
	}

	cov, blocker := authorCoverage(repoDir, sha, shape.Dir, minCover)
	if blocker != "" {
		return skip(blocker)
	}

	return Manifest{
		Repo: repoName, SHA: sha, Kind: "author",
		Prompt:  authorPrompt(pkg, shape.Dir, shape.Tests),
		GoFiles: len(shape.Impls) + len(shape.Tests),
		Author:  &AuthorSpec{Pkg: pkg, Dir: shape.Dir, Coverage: cov, TestFiles: testFiles},
	}, true
}

// authorCoverage runs the ground-truth commit's own test suite in a
// disposable worktree and measures its statement coverage: the expensive
// gate, so it runs last and only on structural survivors. The worktree
// is always removed, success or failure.
func authorCoverage(repoDir, sha, dir string, minCover float64) (float64, string) {
	tmp, err := os.MkdirTemp("", "ago-bench-author-*")
	if err != nil {
		return 0, fmt.Sprintf("coverage worktree: %v", err)
	}
	defer os.RemoveAll(tmp)
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", tmp, sha).CombinedOutput(); err != nil {
		return 0, fmt.Sprintf("worktree add failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer exec.Command("git", "-C", repoDir, "worktree", "remove", "--force", tmp).Run()

	dl := exec.Command("go", "mod", "download")
	dl.Dir = tmp
	if out, err := dl.CombinedOutput(); err != nil {
		return 0, fmt.Sprintf("go mod download failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), authorCoverageTimeout)
	defer cancel()
	test := exec.CommandContext(ctx, "go", "test", "-cover", "./"+dir)
	test.Dir = tmp
	out, _ := test.CombinedOutput() // exit status alone is not the gate; the coverage number is
	cov, ok := parseCoverage(string(out))
	if !ok {
		return 0, fmt.Sprintf("coverage unparseable: %s", strings.TrimSpace(string(out)))
	}
	if cov < minCover {
		return cov, fmt.Sprintf("coverage %.1f below %.0f", cov, minCover)
	}
	return cov, ""
}

// diffTreeFiles lists every path the commit touched, not just Go files:
// authorShapeOf needs the full set to see go.mod edits and non-Go
// passengers.
func diffTreeFiles(repo, sha string) []string {
	out, err := exec.Command("git", "-C", repo, "diff-tree", "--no-commit-id", "--name-only", "-r", sha).Output()
	if err != nil {
		return nil
	}
	var files []string
	for f := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// dirExistsAt reports whether dir already held content at rev: a
// greenfield task's directory must be new, or the model would be editing
// an existing package instead of authoring one.
func dirExistsAt(repo, rev, dir string) bool {
	out, err := exec.Command("git", "-C", repo, "ls-tree", rev, "--", dir).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// modulePathAt reads the module path from go.mod as it stood at sha.
func modulePathAt(repo, sha string) (string, bool) {
	for line := range strings.SplitSeq(gitShow(repo, sha+":go.mod"), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// countLines counts the lines in a file's contents at a revision, for the
// impl-lines cap.
func countLines(src string) int {
	if src == "" {
		return 0
	}
	n := strings.Count(src, "\n")
	if !strings.HasSuffix(src, "\n") {
		n++
	}
	return n
}
