package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task 8: composable decl ops (rename/set_body/add_param/upsert_decl folded
// into opRegistry) plus the new delete_decl/set_doc/add_field/remove_field.

// The classic case atomic multi-rename exists for: an interface method and
// its implementation's method must be renamed together. Renaming either
// alone breaks Impl's satisfaction of Iface (Bind's "return Impl{}" needs
// Impl to implement Iface), so each single rename rejects; the pair, in one
// patch, validates against the final state and accepts.
func TestPatchInterfaceImplRenameAtomic(t *testing.T) {
	s := demo(t)
	for _, decl := range []string{
		"type Iface interface {\n\tM(int) int\n}",
		"type Impl struct{}",
		"func (Impl) M(v int) int { return v }",
		"func Bind() Iface { return Impl{} }",
	} {
		if _, err := s.UpsertDecl("demo/lib", decl); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Iface.M","to":"N"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" {
		t.Fatalf("want typecheck reject for lone Iface.M rename, got %v", err)
	}

	_, err = s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Impl.M","to":"N"}]}`))
	rej, ok = err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" {
		t.Fatalf("want typecheck reject for lone Impl.M rename, got %v", err)
	}

	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Iface.M","to":"N"},
		       {"op":"rename","sym":"Impl.M","to":"N"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Iface.N"); err != nil {
		t.Errorf("renamed interface method not queryable: %v", err)
	}
	if _, err := s.inspect("demo/lib", "Impl.N"); err != nil {
		t.Errorf("renamed impl method not queryable: %v", err)
	}
}

// Two renames in one patch that land in the same file with different
// name-length deltas. Alpha->AlphaLonger (+6) sits first; Beta->B (-3) sits
// second and, in its body, calls Alpha. Beta's own declaration offset must
// account for Alpha's own edits shifting it forward; Alpha's in-body
// reference (inside Beta) must account for Beta's edit shifting it backward
// too. Before the fix, each rename computed its expected post-edit
// positions from only its own edits against pristine bytes, blind to the
// sibling rename's shift in the same file, so verifyResolution
// false-rejected with "reference captured by another declaration" even
// though the composed splice was correct.
func TestPatchSameFileMultiRenameDifferentLengths(t *testing.T) {
	s := demo(t)
	for _, decl := range []string{
		"func Alpha() int {\n\treturn 1\n}",
		"func Beta() int {\n\treturn Alpha()\n}",
	} {
		if _, err := s.UpsertDecl("demo/lib", decl); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Alpha","to":"AlphaLonger"},
		       {"op":"rename","sym":"Beta","to":"B"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "AlphaLonger"); err != nil {
		t.Errorf("renamed symbol not queryable: %v", err)
	}
	if _, err := s.inspect("demo/lib", "B"); err != nil {
		t.Errorf("renamed symbol not queryable: %v", err)
	}
}

// A single rename, and a decl op mixed with a statement op in different
// files, both now flow through the same composable pipeline the old
// "not yet composable" reject used to block.
func TestPatchComposesDeclAndStmtOps(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"n1","where":"after","expr":"helper(9)"},
		       {"op":"rename","sym":"Double","to":"Twice"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Twice"); err != nil {
		t.Errorf("renamed symbol not queryable: %v", err)
	}
}

// A decl op and a statement op compose when they touch different files, but
// a patch that would have both edit the SAME file is rejected: statement ops
// edit ctx.src incrementally and re-parse for handles, decl ops replay a
// per-file ledger over ctx.src, and the two models only reconcile across
// distinct files (see applyDeclEdits' ceiling comment). helper and UseHelper
// both live in use.go, so add_param on helper (rewrites its signature and the
// helper(2) call inside UseHelper) collides with an add_call inside UseHelper.
func TestPatchMixedLegacyAndStmtOps(t *testing.T) {
	// Cross-file: add_param on helper touches use.go; rename Double touches
	// lib.go + main.go; the add_call statement op edits UseHelper in use.go.
	// helper's param edit lands in use.go too, which IS ctx.file — so even
	// this must reject. Use a decl op that stays clear of use.go instead:
	// rename Double (lib.go, main.go) alongside a stmt op in UseHelper.
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"rename","sym":"Double","to":"Twice"},
		       {"op":"add_call","at":"n1","where":"after","expr":"helper(9)"}]}`))
	if err != nil {
		t.Fatalf("cross-file decl+stmt should compose: %v", err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}

	// Same-file: add_param on helper edits use.go, which is also where the
	// add_call statement op works — rejected with the documented ceiling,
	// regardless of op order.
	for _, ops := range []string{
		`[{"op":"add_param","sym":"helper","name":"scale","type":"int","default":"1"},
		  {"op":"add_call","at":"n1","where":"after","expr":"helper(9, 2)"}]`,
		`[{"op":"add_call","at":"n1","where":"after","expr":"helper(9)"},
		  {"op":"add_param","sym":"helper","name":"scale","type":"int","default":"1"}]`,
	} {
		s := demo(t)
		_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","ops":` + ops + `}`))
		rej, ok := err.(*Reject)
		if !ok || rej.Reason != "cannot mix a decl op and a statement op on the same file" {
			t.Fatalf("want same-file ceiling reject, got %v", err)
		}
	}
}

func TestPatchSetBody(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"set_body","body":"return v + v"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), "return v + v") {
		t.Errorf("edit not written:\n%s", b)
	}
}

func TestPatchAddParam(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"add_param","name":"scale","type":"int","default":"1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	lib, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(lib), "func Double(v int, scale int) int") {
		t.Errorf("declaration not updated:\n%s", lib)
	}
	main, _ := os.ReadFile(filepath.Join(s.dir, "main.go"))
	if !strings.Contains(string(main), "lib.Double(lib.Limit, 1)") {
		t.Errorf("caller not updated:\n%s", main)
	}
}

// upsert_decl composes for the two cases that don't need a fresh file:
// replacing an existing declaration in place, and appending to an agent.go
// that already exists.
func TestPatchUpsertDeclReplacesExisting(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"upsert_decl","text":"func Double(v int) int {\n\treturn v << 1\n}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), "v << 1") {
		t.Errorf("declaration not replaced:\n%s", b)
	}
}

func TestPatchUpsertDeclAppendsToExistingFile(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Seed() int { return 0 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"upsert_decl","text":"func Triple(v int) int {\n\treturn v * 3\n}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Triple"); err != nil {
		t.Errorf("new decl not queryable: %v", err)
	}
}

// New decls in an existing package append to an existing file through the
// ledger (the landing move_decl uses), never a mid-patch created agent.go:
// ledger edits validate at end-of-list, so a new decl can reference other
// in-flight ops; a created file validates against a disk reload that
// cannot see the ledger.
func TestPatchUpsertDeclAppendsToExistingPackageFile(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"upsert_decl","text":"func Triple(v int) int {\n\treturn v * 3\n}"},
		       {"op":"upsert_decl","text":"func Nonuple(v int) int {\n\treturn Triple(Triple(v))\n}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); serr == nil {
		t.Fatal("mid-patch upsert created agent.go in a package with files")
	}
	if _, err := s.inspect("demo/lib", "Nonuple"); err != nil {
		t.Errorf("new decl not queryable: %v", err)
	}
}

// A later op's rejection must roll the appended decls back.
func TestPatchUpsertDeclNewFileRejectionCleansUp(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"upsert_decl","text":"func Triple(v int) int {\n\treturn v * 3\n}"},
		       {"op":"upsert_decl","text":"func Broken() int {\n\treturn undefinedIdent\n}"}]}`))
	if err == nil {
		t.Fatal("want rejection")
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); serr == nil {
		t.Fatal("agent.go survived a rejected patch")
	}
	if b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go")); strings.Contains(string(b), "Triple") {
		t.Fatal("rejected patch left Triple behind")
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("snapshot broken after cleanup: %v", err)
	}
}

// A brand-new package mid-patch composes with ops that need the package to
// exist: create demo/util, then move Double into it, one atomic patch.
func TestPatchUpsertDeclCreatesNewPackage(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/util",
		"ops":[{"op":"upsert_decl","text":"func Seed() int {\n\treturn 1\n}"},
		       {"op":"move_decl","pkg":"demo/lib","sym":"Double","to_pkg":"demo/util"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/util", "Double"); err != nil {
		t.Errorf("moved decl not queryable in new package: %v", err)
	}
	if _, err := s.inspect("demo/lib", "Double"); err == nil {
		t.Error("Double still in demo/lib")
	}
}

// A rejected patch must remove the created package file; the directory a
// failed create leaves behind is harmless (Go ignores .go-less dirs).
func TestPatchUpsertDeclNewPackageRejectionCleansUp(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/util",
		"ops":[{"op":"upsert_decl","text":"func Seed() int {\n\treturn 1\n}"},
		       {"op":"upsert_decl","text":"func Broken() int {\n\treturn undefinedIdent\n}"}]}`))
	if err == nil {
		t.Fatal("want rejection")
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "util", "agent.go")); serr == nil {
		t.Fatal("util/agent.go survived a rejected patch")
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("snapshot broken after cleanup: %v", err)
	}
}

func TestPatchUpsertDeclNewFileDryRun(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","dry_run":true,
		"ops":[{"op":"upsert_decl","text":"func Triple(v int) int {\n\treturn v * 3\n}"}]}`))
	if err != nil || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); serr == nil {
		t.Fatal("dry_run left agent.go on disk")
	}
}

func TestPatchDryRunRename(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double","dry_run":true,
		"ops":[{"op":"rename","to":"Twice"}]}`))
	if err != nil || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if string(orig) != string(after) {
		t.Fatal("dry_run wrote")
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("dry_run renamed the symbol: %v", err)
	}
}

// delete_decl

func TestDeleteDeclRejectsWhileReferenced(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"delete_decl","sym":"Double"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol is still referenced" || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteDeclAccepts(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Unused() int { return 1 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"delete_decl","sym":"Unused"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Unused"); err == nil {
		t.Error("deleted decl still queryable")
	}
}

// A recursive function's own self-calls are references to itself that live
// entirely inside its own declaration range; they must not count as "still
// referenced" and block the delete. Nothing outside Fib calls it.
func TestDeleteDeclRecursive(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib",
		"func Fib(n int) int {\n\tif n < 2 {\n\t\treturn n\n\t}\n\treturn Fib(n-1) + Fib(n-2)\n}"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"delete_decl","sym":"Fib"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Fib"); err == nil {
		t.Error("deleted decl still queryable")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	if strings.Contains(string(b), "Fib") {
		t.Fatalf("recursive decl not deleted:\n%s", b)
	}
}

// set_doc

func TestSetDoc(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"set_doc","text":"Double doubles v."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), "// Double doubles v.\nfunc Double") {
		t.Fatalf("doc comment not created:\n%s", b)
	}

	// Replacing an existing doc comment must remove the old one, not append.
	_, err = s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"set_doc","text":"Now doubled differently."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if strings.Contains(string(b), "doubles v") || !strings.Contains(string(b), "// Now doubled differently.\nfunc Double") {
		t.Fatalf("doc comment not replaced:\n%s", b)
	}
}

func TestSetDocRejectsMissingSymbol(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Nope",
		"ops":[{"op":"set_doc","text":"whatever"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol not found" {
		t.Fatalf("got %v", err)
	}
}

// add_field / remove_field

func TestAddRemoveField(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Store",
		"ops":[{"op":"add_field","name":"Tag","type":"string"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), "Tag string") {
		t.Fatalf("field not added:\n%s", b)
	}
	if _, err := s.inspect("demo/lib", "Store.Tag"); err != nil {
		t.Errorf("new field not queryable: %v", err)
	}

	res, err = s.Patch([]byte(`{"pkg":"demo/lib","sym":"Store.Tag",
		"ops":[{"op":"remove_field"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ = os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if strings.Contains(string(b), "Tag string") {
		t.Fatalf("field not removed:\n%s", b)
	}
	if _, err := s.inspect("demo/lib", "Store.Tag"); err == nil {
		t.Error("removed field still queryable")
	}
}

func TestAddFieldRejectsDuplicate(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Store",
		"ops":[{"op":"add_field","name":"n","type":"int"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "field already exists" {
		t.Fatalf("got %v", err)
	}
}

func TestRemoveFieldRejectsWhileReferenced(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Store.n",
		"ops":[{"op":"remove_field"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "field is still referenced" || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
}

func TestRemoveFieldRejectsNonField(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Store.Put",
		"ops":[{"op":"remove_field"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol is not a field" {
		t.Fatalf("got %v", err)
	}
}

// upsert_decl must find and replace a declaration living in a _test.go
// file: moved test funcs land in test files, and a recipe upserting their
// rewritten bodies would otherwise append a duplicate to agent.go.
func TestPatchUpsertDeclReplacesTestFileDecl(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig",
		"ops":[{"op":"upsert_decl","text":"func TestSelfContained(t *testing.T) {\n\tif 2+2 != 4 {\n\t\tt.Fatal(\"arithmetic broke harder\")\n\t}\n}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "sig", "sig_test.go"))
	if !strings.Contains(string(b), "arithmetic broke harder") {
		t.Errorf("test-file decl not replaced in place:\n%s", b)
	}
	if strings.Contains(string(b), "if 1+1 != 2") {
		t.Errorf("old body still present:\n%s", b)
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, "sig", "agent.go")); err == nil && strings.Contains(string(b), "TestSelfContained") {
		t.Errorf("duplicate appended to agent.go:\n%s", b)
	}
}

// A brand-new Test func lands in a _test.go file, mirroring move_decl's
// landing rule: a test in a non-test file compiles but never runs, which
// reads as green while executing nothing.
func TestPatchUpsertDeclNewTestFuncLandsInTestFile(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig",
		"ops":[{"op":"upsert_decl","text":"func TestShoutUpper(t *testing.T) {\n\tif Shout(\"a\") != \"A\" {\n\t\tt.Fatal(\"not shouted\")\n\t}\n}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	found := false
	for _, f := range []string{"sig_test.go", "agent_test.go"} {
		if b, err := os.ReadFile(filepath.Join(s.dir, "sig", f)); err == nil && strings.Contains(string(b), "TestShoutUpper") {
			found = true
		}
	}
	if !found {
		t.Error("new test func not in a _test.go file")
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, "sig", "agent.go")); err == nil && strings.Contains(string(b), "TestShoutUpper") {
		t.Errorf("test func landed in non-test agent.go:\n%s", b)
	}
}

// upsert_decl carries explicitly named imports into the landing file:
// goimports cannot infer an aliased import (stderrors "errors"), and no
// other op expresses one (boundary 687dd1bd's rewritten Run body).
func TestPatchUpsertDeclCarriesImports(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig",
		"ops":[{"op":"upsert_decl","text":"func WrapEOF() error {\n\treturn stde.New(\"eof\")\n}",
		        "imports":[{"path":"errors","name":"stde"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	var landed []byte
	for _, f := range []string{"agent.go", "sig.go", "imports.go", "consts.go"} {
		if b, err := os.ReadFile(filepath.Join(s.dir, "sig", f)); err == nil && strings.Contains(string(b), "WrapEOF") {
			landed = b
			break
		}
	}
	if landed == nil {
		t.Fatal("WrapEOF landed nowhere")
	}
	if !strings.Contains(string(landed), `stde "errors"`) {
		t.Errorf("aliased import not carried:\n%s", landed)
	}
}

// delete_decl composes with an earlier op that removes the last reference:
// the still-referenced guard must not count references inside spans the
// patch ledger already replaced (687dd1bd: registerJobs rewritten away from
// the helper, then the helper deleted, one atomic patch).
func TestPatchDeleteDeclAfterReferenceRewritten(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"set_body","sym":"UseFetch","body":"return 1"},
		{"op":"set_body","sym":"SpreadFetch","body":"return len(nums)"},
		{"op":"delete_decl","sym":"Fetch"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/sig", "Fetch"); err == nil {
		t.Error("Fetch still present")
	}
}

// delete_decl batches like move_decl: symbols that reference each other
// delete together where one-at-a-time would reject on the intra-set
// reference (687dd1bd deletes an option func and the test that used it).
func TestPatchDeleteDeclBatchIntraSetReference(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"set_body","sym":"UseFetch","body":"return 1"},
		{"op":"delete_decl","syms":["Fetch","SpreadFetch"]}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	for _, sym := range []string{"Fetch", "SpreadFetch"} {
		if _, err := s.inspect("demo/sig", sym); err == nil {
			t.Errorf("%s still present", sym)
		}
	}
}

// A delete_decl whose span contains an earlier op's smaller edit (the move
// requalified a reference inside the soon-deleted helper) supersedes it:
// replaying both corrupts the splice (687dd1bd: "expected declaration,
// found rn" from a mid-token cut).
func TestPatchDeleteDeclSupersedesContainedEdits(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/lib"},
		{"op":"delete_decl","sym":"SpreadFetch"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/sig", "SpreadFetch"); err == nil {
		t.Error("SpreadFetch still present")
	}
	if _, err := s.inspect("demo/lib", "Fetch"); err != nil {
		t.Errorf("Fetch not moved: %v", err)
	}
}

// delete_decl excises one spec from a grouped const/var block, siblings
// and group intact (7ec1fe75 deletes members of vault's const groups).
func TestPatchDeleteDeclGroupedMember(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"delete_decl","sym":"ModeSlow"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "sig", "consts.go"))
	if strings.Contains(string(b), "ModeSlow") {
		t.Errorf("ModeSlow not excised:\n%s", b)
	}
	if !strings.Contains(string(b), "ModeFast") {
		t.Errorf("sibling lost:\n%s", b)
	}
	if _, err := s.inspect("demo/sig", "ModeFast"); err != nil {
		t.Errorf("sibling not queryable: %v", err)
	}
}

// A member of an iota group cannot be deleted: position defines siblings'
// values. Reject with the blocker named, never a silent value shift.
func TestPatchDeleteDeclIotaGroupRejects(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"delete_decl","sym":"LevelA"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason+rej.Detail, "iota") {
		t.Fatalf("want iota reject, got %v", err)
	}
}

// upsert_decl replaces one spec inside a grouped block in place: the
// incoming text is a standalone single decl, the landing keeps the group.
func TestPatchUpsertDeclGroupedMember(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"upsert_decl","text":"// ModeSlow is now leisurely.\nconst ModeSlow = \"leisurely\""}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "sig", "consts.go"))
	if !strings.Contains(string(b), `ModeSlow = "leisurely"`) {
		t.Errorf("spec not replaced:\n%s", b)
	}
	if !strings.Contains(string(b), "now leisurely") {
		t.Errorf("doc comment lost:\n%s", b)
	}
	if strings.Count(string(b), "ModeSlow") != 2 { // doc + spec
		t.Errorf("duplicate or stray ModeSlow:\n%s", b)
	}
	if !strings.Contains(string(b), "ModeFast") {
		t.Errorf("sibling lost:\n%s", b)
	}
}

// Replacing a grouped member with a different token kind is incoherent
// inside the group; reject with the mismatch named.
func TestPatchUpsertDeclGroupedTokenMismatchRejects(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"upsert_decl","text":"var ModeSlow = \"slow\""}]}`))
	rej, ok := err.(*Reject)
	if !ok || !(strings.Contains(rej.Reason+rej.Detail, "const") || strings.Contains(rej.Reason+rej.Detail, "group")) {
		t.Fatalf("want token-mismatch reject, got %v", err)
	}
}

// Replacing a grouped member whose FOLLOWING spec inherits its expression
// would silently change the follower's value; reject with the dependency
// named (a silent wrong answer is the bug).
func TestPatchUpsertDeclGroupedInheritedFollowerRejects(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"upsert_decl","text":"const SizeBig = 12"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason+rej.Detail, "SizeHuge") {
		t.Fatalf("want inherited-follower reject naming SizeHuge, got %v", err)
	}
}

// Deleting a type and its methods while a surviving caller's receiver
// type is swapped in the same patch (7ec1fe75: ClusterListener ->
// cluster.Listener under Core's field): the method's still-referenced
// guard must not block on a reference the final state resolves to the
// replacement type; the end-of-list typecheck is the arbiter.
func TestPatchDeleteDeclTypeMigrationBatch(t *testing.T) {
	s := demo(t)
	for _, decl := range []string{
		"type Old struct{}",
		"func (Old) Ping() int { return 1 }",
		"type Successor struct{}",
		"func (Successor) Ping() int { return 2 }",
		"type Box struct{ p Old }",
		"func (b Box) Use() int { return b.p.Ping() }",
	} {
		if _, err := s.UpsertDecl("demo/lib", decl); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","ops":[
		{"op":"upsert_decl","text":"type Box struct{ p Successor }"},
		{"op":"delete_decl","syms":["Old","Old.Ping"]}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Old"); err == nil {
		t.Error("Old still present")
	}
	if _, err := s.inspect("demo/lib", "Box.Use"); err != nil {
		t.Errorf("surviving caller lost: %v", err)
	}
}

// An empty module is where authoring starts: upsert_decl must create the
// first package when the workspace has zero loaded packages, resolving
// the module from go.mod directly.
func TestUpsertDeclIntoEmptyModule(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch.local/kvd\n\ngo 1.24\n"), 0o644)
	s := New(dir)
	if _, err := s.UpsertDecl("scratch.local/kvd/kv", "func Get(k string) string { return k }"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "kv", "agent.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "package kv\n") {
		t.Fatalf("created file:\n%s", b)
	}
	// The package exists now; the next decl takes the normal path.
	if _, err := s.UpsertDecl("scratch.local/kvd/kv", "func Put(k string) string { return Get(k) }"); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertDeclEmptyModuleBareNameSuggestsModulePath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch.local/kvd\n\ngo 1.24\n"), 0o644)
	s := New(dir)
	_, err := s.UpsertDecl("kv", "func Get() {}")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "package not found" {
		t.Fatalf("got %v", err)
	}
	found := false
	for _, c := range rej.DidYouMean {
		if c == "scratch.local/kvd/kv" {
			found = true
		}
	}
	if !found {
		t.Fatalf("did_you_mean = %v, want module-prefixed completion", rej.DidYouMean)
	}
}

// A brand-new package whose first declaration is func main is a command;
// the package clause must be main, not the directory name.
func TestUpsertDeclFuncMainNamesPackageMain(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch.local/kvd\n\ngo 1.24\n"), 0o644)
	s := New(dir)
	if _, err := s.UpsertDecl("scratch.local/kvd/cmd/kvd", "func main() {}"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cmd", "kvd", "agent.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "package main\n") {
		t.Fatalf("created file:\n%s", b)
	}
}

func TestPatchUpsertDeclIntoEmptyModule(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch.local/kvd\n\ngo 1.24\n"), 0o644)
	s := New(dir)
	res, err := s.Patch([]byte(`{"pkg":"scratch.local/kvd/kv",
		"ops":[{"op":"upsert_decl","text":"func Get(k string) string { return k }"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "kv", "agent.go")); err != nil {
		t.Fatal(err)
	}
}
