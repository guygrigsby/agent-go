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

func TestAuthorOracleOps(t *testing.T) {
	files := map[string]string{
		"echo/echo.go": "package echo\n\nimport \"fmt\"\n\n// Echo repeats s.\nfunc Echo(s string) string { return fmt.Sprint(s) }\n\nconst limit = 10\n",
		"echo/aux.go":  "package echo\n\nfunc aux() {}\n",
	}
	ops, err := authorOracleOps("example.com/m/echo", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("want 3 upsert ops (import blocks are the engine's job), got %d: %v", len(ops), ops)
	}
	for _, op := range ops {
		if op["op"] != "upsert_decl" || op["pkg"] != "example.com/m/echo" {
			t.Fatalf("op shape drifted: %v", op)
		}
	}
	// File order is sorted, so aux comes first; the Echo decl carries its
	// doc comment (Rename and set_doc conventions depend on docs moving
	// with decls).
	if !strings.Contains(ops[1]["text"].(string), "// Echo repeats s.") {
		t.Fatalf("doc comment must ride with the decl: %v", ops[1]["text"])
	}
}
