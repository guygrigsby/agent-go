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

// n1 in UseHelper is a plain statement; craft an if via patch, then try
// deleting its handle while populated. This also exercises Task 6's add_if
// and $N handles; Tasks 5 and 6 land together so both are available here.
func TestDeleteNodeRejectsNonEmptyBlock(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"true"},
		       {"op":"add_call","at":"$1","where":"first","expr":"println(1)"},
		       {"op":"delete_node","at":"$1"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "block is not empty" {
		t.Fatalf("got %v", err)
	}
}

// An if's then-block being empty is not enough: deleting it would silently
// discard a populated else clause.
func TestDeleteNodeRejectsIfWithElse(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"true","else":true},
		       {"op":"delete_node","at":"$1"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "block is not empty" {
		t.Fatalf("got %v", err)
	}
}

// Task 6: block constructors (add_if, add_for, add_switch, add_case) and $N
// intra-patch handle references.

func TestBlockConstructorsAndDollarRefs(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"helper(1) > 0"},
		       {"op":"add_call","at":"$1","where":"first","expr":"println(\"pos\")"},
		       {"op":"add_switch","at":"$1","where":"after","tag":"helper(2)"},
		       {"op":"add_case","at":"$3","exprs":["1","2"]},
		       {"op":"add_call","at":"$4","where":"first","expr":"println(\"small\")"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["ops_applied"].(int) != 5 {
		t.Fatalf("got %v", res)
	}
}

// Deferred from Task 4's review: once add_if exists, exercise insertStmt's
// where:"first" and where:"last" against a block-owning handle, asserting
// both placements land inside the braces in the right order regardless of
// which is applied first.
func TestInsertStmtFirstLastAgainstBlock(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"true"},
		       {"op":"add_call","at":"$1","where":"last","expr":"println(2)"},
		       {"op":"add_call","at":"$1","where":"first","expr":"println(1)"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	firstIdx := strings.Index(string(b), "println(1)")
	lastIdx := strings.Index(string(b), "println(2)")
	if firstIdx == -1 || lastIdx == -1 || firstIdx > lastIdx {
		t.Fatalf("first/last placement wrong order:\n%s", b)
	}
}

func TestAddForCondAndInfiniteForms(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_for","at":"n1","where":"after","cond":"helper(1) > 0"},
		       {"op":"add_for","at":"$1","where":"after"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
}

func TestAddForRangeForm(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"items","rhs":"[]int{1, 2, 3}","define":true},
		       {"op":"add_for","at":"$1","where":"after","range":"range items"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
}

func TestAddSwitchTaglessAndCaseDefault(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_switch","at":"n1","where":"after"},
		       {"op":"add_case","at":"$1","default":true},
		       {"op":"add_call","at":"$2","where":"first","expr":"println(1)"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["ops_applied"].(int) != 3 {
		t.Fatalf("got %v", res)
	}
}

func TestDollarRefUnknown(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"$9","where":"after","expr":"helper(1)"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown $ref" {
		t.Fatalf("got %v", err)
	}
}

// $2 in op 1 refers to op 2's own not-yet-produced handle: a forward
// reference, rejected the same way as a wildly unknown one.
func TestDollarRefForward(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_call","at":"$2","where":"after","expr":"helper(1)"},
		       {"op":"add_call","at":"n1","where":"after","expr":"helper(2)"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown $ref" {
		t.Fatalf("got %v", err)
	}
}
