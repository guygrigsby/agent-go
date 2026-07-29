package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestAuthorOracleOpsOrdering covers the boundary_86f2535a shape: an
// exported decl that calls a helper declared later in the same file. The
// first op in a package with no existing files takes createFileInPatch,
// which validates immediately with no ledger visibility, so the first op
// must be self-contained; ops[1:] append and typecheck at end-of-patch, so
// only the dependency edge into the first op is load-bearing. The helper
// must land first.
func TestAuthorOracleOpsOrdering(t *testing.T) {
	files := map[string]string{
		"ord/ord.go": "package ord\n\nfunc Public() int { return helper() }\n\nfunc helper() int { return 1 }\n",
	}
	ops, err := authorOracleOps("example.com/m/ord", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 ops, got %d: %v", len(ops), ops)
	}
	if !strings.Contains(ops[0]["text"].(string), "func helper") {
		t.Fatalf("dependency must land first, got order: %v / %v", ops[0]["text"], ops[1]["text"])
	}
	if !strings.Contains(ops[1]["text"].(string), "func Public") {
		t.Fatalf("dependent must land after its dependency: %v / %v", ops[0]["text"], ops[1]["text"])
	}
}

// TestAuthorOracleOpsCycle covers mutual recursion: neither decl has zero
// unresolved forward references, so no order eliminates the first op's
// exposure; the tie-break falls back to source order. The load-bearing
// assertions are that both ops still land (no drop) and the call returns
// promptly (no infinite loop hunting for a nonexistent zero-indegree node).
func TestAuthorOracleOpsCycle(t *testing.T) {
	files := map[string]string{
		"cyc/cyc.go": "package cyc\n\nfunc A() { B() }\n\nfunc B() { A() }\n",
	}
	type result struct {
		ops []map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		ops, err := authorOracleOps("example.com/m/cyc", files)
		done <- result{ops, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.ops) != 2 {
			t.Fatalf("cycle must not drop a decl, want 2 ops, got %d: %v", len(r.ops), r.ops)
		}
		if !strings.Contains(r.ops[0]["text"].(string), "func A") ||
			!strings.Contains(r.ops[1]["text"].(string), "func B") {
			t.Fatalf("cycle keeps source order, got: %v / %v", r.ops[0]["text"], r.ops[1]["text"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authorOracleOps did not return: suspect infinite loop on a reference cycle")
	}
}

// TestAuthorOracleOpsImports covers the goimports-ambiguity half of the
// bug: each file's own import block (path plus alias, alias empty for the
// default name) rides on that file's ops, so an explicit imports field
// resolves what goimports would otherwise guess wrong (e.g. a bare "proto"
// resolving to the wrong module). A file with no imports emits ops with no
// imports field at all.
func TestAuthorOracleOpsImports(t *testing.T) {
	files := map[string]string{
		"imp/imp.go":   "package imp\n\nimport (\n\t\"fmt\"\n\n\tio2 \"io\"\n)\n\nfunc F() { fmt.Println(io2.EOF) }\n",
		"imp/plain.go": "package imp\n\nfunc G() {}\n",
	}
	ops, err := authorOracleOps("example.com/m/imp", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 ops, got %d: %v", len(ops), ops)
	}
	// imp.go sorts before plain.go, so ops[0] is F, ops[1] is G; neither
	// references the other so the dependency-order tie-break keeps file
	// order.
	fOp, gOp := ops[0], ops[1]
	if !strings.Contains(fOp["text"].(string), "func F") {
		t.Fatalf("expected F first, got: %v", fOp["text"])
	}
	imps, ok := fOp["imports"].([]importRef)
	if !ok {
		t.Fatalf("F's op must carry imp.go's import block as []importRef, got %T: %v", fOp["imports"], fOp["imports"])
	}
	want := []importRef{{Path: "fmt"}, {Path: "io", Name: "io2"}}
	if len(imps) != len(want) || imps[0] != want[0] || imps[1] != want[1] {
		t.Fatalf("imports mismatch, got %v want %v", imps, want)
	}
	if !strings.Contains(gOp["text"].(string), "func G") {
		t.Fatalf("expected G second, got: %v", gOp["text"])
	}
	if _, ok := gOp["imports"]; ok {
		t.Fatalf("plain.go has no imports, op must not carry the field: %v", gOp["imports"])
	}
}
