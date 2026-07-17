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
	// install.sh is the one named exception: a bootstrap installer cannot
	// be a subcommand of the binary it installs.
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f == "install.sh" {
			continue
		}
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

// The distributed skill (skills/ago) teaches CLI invocations; every op it
// names must be dispatched and every flag registered, same contract as
// the README guard.
func TestSkillCommandsResolve(t *testing.T) {
	raw, err := os.ReadFile("../../skills/ago/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	inline := regexp.MustCompile("`ago ([^`]+)`")
	ms := inline.FindAllStringSubmatch(string(raw), -1)
	if len(ms) < 5 {
		t.Fatalf("suspiciously few ago invocations in the skill: %d", len(ms))
	}
	for _, m := range ms {
		cmd := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(m[1], "<", ""), ">", ""))
		for _, p := range commandProblems(cmd) {
			t.Errorf("skill invocation %q: %s", "ago "+m[1], p)
		}
	}
}
