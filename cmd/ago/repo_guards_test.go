package main

import (
	"os/exec"
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
