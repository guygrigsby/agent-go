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
		       {"op":"add_call","at":"n3","where":"before","expr":"_ = extra"}]}`))
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
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","dry_run":true,
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"x","rhs":"1","define":true}]}`))
	if err != nil || res["dry_run"] != true || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("dry_run wrote")
	}
}
