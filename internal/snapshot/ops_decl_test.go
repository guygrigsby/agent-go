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
	if _, err := s.Inspect("demo/lib", "Iface.N"); err != nil {
		t.Errorf("renamed interface method not queryable: %v", err)
	}
	if _, err := s.Inspect("demo/lib", "Impl.N"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "AlphaLonger"); err != nil {
		t.Errorf("renamed symbol not queryable: %v", err)
	}
	if _, err := s.Inspect("demo/lib", "B"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "Twice"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "Triple"); err != nil {
		t.Errorf("new decl not queryable: %v", err)
	}
}

// Creating a brand-new agent.go mid-patch mirrors add_test's new-file path:
// write and reload immediately, register in createdFiles so every failure
// path cleans up. Later ops in the same patch compose against the reloaded
// snapshot (the second upsert_decl below appends to the just-created file
// through the ledger).
func TestPatchUpsertDeclCreatesNewFile(t *testing.T) {
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
	b, err := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	if err != nil || !strings.Contains(string(b), "Triple") || !strings.Contains(string(b), "Nonuple") {
		t.Fatalf("agent.go missing decls: %v\n%s", err, b)
	}
	if _, err := s.Inspect("demo/lib", "Nonuple"); err != nil {
		t.Errorf("new decl not queryable: %v", err)
	}
}

// A later op's rejection must not leave the created file behind.
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
	if _, err := s.Inspect("demo/lib", "Double"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "Double"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "Unused"); err == nil {
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
	if _, err := s.Inspect("demo/lib", "Fib"); err == nil {
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
	if _, err := s.Inspect("demo/lib", "Store.Tag"); err != nil {
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
	if _, err := s.Inspect("demo/lib", "Store.Tag"); err == nil {
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
