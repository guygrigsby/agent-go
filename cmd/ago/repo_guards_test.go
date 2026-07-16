package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Tenet 9 (tooling speaks the project's language): a committed script is
// a missing subcommand. Mining, reporting, extraction, and validation are
// Go code with tests. When this fails, port the script to a subcommand
// under cmd/ or delete it.
func TestNoSidecarScripts(t *testing.T) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, ext := range []string{".py", ".sh", ".rb", ".pl", ".js"} {
			if strings.HasSuffix(f, ext) {
				t.Errorf("committed script %s: port it to a Go subcommand (tenet 9)", f)
			}
		}
	}
}

// Tenet 1 applied to CLAUDE.md itself: every path its doc map and
// architecture overview reference must exist, or the onboarding doc rots.
func TestClaudeMdPathsExist(t *testing.T) {
	raw, err := os.ReadFile("../../CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	path := regexp.MustCompile("`((?:docs|internal|cmd|bench)/[a-zA-Z0-9._/-]*|idea.md)`")
	seen := map[string]bool{}
	for _, m := range path.FindAllStringSubmatch(string(raw), -1) {
		p := strings.TrimSuffix(m[1], "/")
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat("../../" + p); err != nil {
			t.Errorf("CLAUDE.md references %s, which does not exist", p)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("only %d paths matched; extraction broken?", len(seen))
	}
}
