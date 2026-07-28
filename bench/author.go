package bench

// Author kind helpers: the ground-truth commit's tests are the spec
// (docs/specs/authoring-bench.md). The harness lands them pre-episode and
// scoring demands them back byte for byte; deleting or editing the test
// is the shortest path to green, so the freeze fails the episode
// outright.

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeAuthorSpec lands the frozen spec tests in a fresh worktree.
func writeAuthorSpec(wt string, spec *AuthorSpec) error {
	for _, f := range spec.TestFiles {
		abs := filepath.Join(wt, f.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// specFilesIntact reports whether every spec test file still carries its
// frozen bytes; reason names the first divergence.
func specFilesIntact(wt string, spec *AuthorSpec) (string, bool) {
	for _, f := range spec.TestFiles {
		b, err := os.ReadFile(filepath.Join(wt, f.Path))
		if err != nil {
			return fmt.Sprintf("spec test %s is gone: %v", f.Path, err), false
		}
		if string(b) != f.Content {
			return fmt.Sprintf("spec test %s was modified", f.Path), false
		}
	}
	return "", true
}
