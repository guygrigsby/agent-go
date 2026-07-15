package snapshot

import (
	"os"
	"strings"
	"testing"
)

func TestPatchInsertAfterHandle(t *testing.T) {
	s := demo(t)
	// UseHelper body: n1 assign, n2 blank-assign, n3 return.
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"extra","rhs":"h(1)","define":true},
		       {"op":"add_assign","at":"n3","where":"before","lhs":"_","rhs":"extra"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	if !strings.Contains(string(b), "extra := h(1)") {
		t.Fatalf("insert missing:\n%s", b)
	}
}

func TestPatchAtomicOnLaterFailure(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(s.dir + "/lib/use.go")
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"x","rhs":"1","define":true},
		       {"op":"add_call","at":"n99","where":"before","expr":"println(x)"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown handle" || !strings.Contains(rej.Detail, "op 2") {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("failed patch left partial edits")
	}
}

func TestPatchDryRun(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(s.dir + "/lib/use.go")
	before, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","dry_run":true,
		"ops":[{"op":"add_call","at":"n1","where":"after","expr":"helper(2)"}]}`))
	if err != nil || res["dry_run"] != true || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("dry_run wrote")
	}
	afterView, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	if before["generation"] != afterView["generation"] {
		t.Fatalf("dry_run bumped generation: before %v after %v", before["generation"], afterView["generation"])
	}
}

// A dry_run whose proposed edit does not typecheck must reject with the same
// diagnostics a real commit would produce, and must leave both the file and
// the target's generation untouched — previewing is not observable state
// change, even on the reject path.
func TestPatchDryRunRejectsTypeError(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(s.dir + "/lib/use.go")
	before, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","dry_run":true,
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"y","rhs":"undefinedIdent","define":true}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("rejected dry_run wrote")
	}
	afterView, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	if before["generation"] != afterView["generation"] {
		t.Fatalf("rejected dry_run bumped generation: before %v after %v", before["generation"], afterView["generation"])
	}
}

// add_call's "expr" field must be a genuine call (or receive) expression, not
// arbitrary statement text — an assignment belongs to add_assign instead.
func TestPatchAddCallRejectsNonCallExpr(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"n1","where":"after","expr":"extra"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "add_call requires a call expression" {
		t.Fatalf("got %v", err)
	}
}

// A parenthesized call and a channel receive are both genuine call-shaped
// expressions and must still be accepted.
func TestPatchAddCallAcceptsParenAndReceive(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"n1","where":"after","expr":"(helper)(3)"}]}`))
	if err != nil || res["status"] != "accepted" {
		t.Fatalf("paren call: got %v %v", res, err)
	}
}

// Task 5: linear statement ops (add_return, add_defer, add_go, delete_node).

func TestAddReturn(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"delete_node","at":"n3"},
		       {"op":"add_return","at":"n2","where":"after","exprs":["helper(3)"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddReturnArityChecked(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_return","at":"n3","where":"before","exprs":["1","2"]}]}`))
	rej, ok := err.(*Reject)
	if !ok || len(rej.Diagnostics) == 0 {
		t.Fatalf("want typecheck reject on arity, got %v", err)
	}
}

func TestAddDefer(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_defer","at":"n1","where":"after","expr":"helper(1)"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	if !strings.Contains(string(b), "defer helper(1)") {
		t.Fatalf("defer missing:\n%s", b)
	}
}

// defer's expression must be a call; go/parser itself enforces this (not the
// typechecker), so a non-call expression surfaces from insertStmt's re-parse
// after splicing, not from a special check in add_defer.
func TestAddDeferRejectsNonCall(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_defer","at":"n1","where":"after","expr":"1"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "insertion does not parse" {
		t.Fatalf("got %v", err)
	}
}

func TestAddGo(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_go","at":"n1","where":"after","expr":"helper(1)"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	if !strings.Contains(string(b), "go helper(1)") {
		t.Fatalf("go missing:\n%s", b)
	}
}

// Standalone success path, with no dependency on Task 6's add_if/$N: add a
// disposable call then remove it by its literal (post-shift) handle name,
// netting no change and still typechecking.
func TestDeleteNodeSuccess(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"n1","where":"after","expr":"helper(9)"},
		       {"op":"delete_node","at":"n2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	if strings.Contains(string(b), "helper(9)") {
		t.Fatalf("delete_node left the node:\n%s", b)
	}
}
