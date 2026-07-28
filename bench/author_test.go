package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecFreeze(t *testing.T) {
	wt := t.TempDir()
	spec := &AuthorSpec{Pkg: "example.com/m/echo", Dir: "echo",
		TestFiles: []AuthorFile{{Path: "echo/echo_test.go", Content: "package echo\n"}}}
	if err := writeAuthorSpec(wt, spec); err != nil {
		t.Fatal(err)
	}
	if reason, ok := specFilesIntact(wt, spec); !ok {
		t.Fatalf("freshly placed spec must be intact: %s", reason)
	}
	// Mutation is a spec violation and the reason names the file.
	mutated := filepath.Join(wt, "echo", "echo_test.go")
	os.WriteFile(mutated, []byte("package echo // gutted\n"), 0o644)
	if reason, ok := specFilesIntact(wt, spec); ok || !strings.Contains(reason, "echo/echo_test.go") {
		t.Fatalf("mutated spec must fail naming the file, got ok=%v reason=%q", ok, reason)
	}
	// Deletion too.
	os.Remove(mutated)
	if _, ok := specFilesIntact(wt, spec); ok {
		t.Fatal("deleted spec file must fail the freeze")
	}
}
