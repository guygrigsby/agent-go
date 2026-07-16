package snapshot

import (
	"strings"
	"testing"
)

// move_decl relocates a self-contained function and requalifies every
// reference: callers in the source package gain the target qualifier,
// callers elsewhere swap qualifiers.
func TestMoveDeclFunc(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	// The symbol now lives in demo/lib.
	if _, err := s.Inspect("demo/lib", "Fetch"); err != nil {
		t.Fatalf("Fetch not in target: %v", err)
	}
	if _, err := s.Inspect("demo/sig", "Fetch"); err == nil {
		t.Fatal("Fetch still in source")
	}
	// Source-package callers now qualify.
	out, err := s.View("demo/sig", "UseFetch")
	if err != nil {
		t.Fatal(err)
	}
	if text := out["text"].(string); !strings.Contains(text, "lib.Fetch(1, \"x\", 2, 3)") {
		t.Fatalf("source caller not requalified:\n%s", text)
	}
}

// A declaration that leans on package-local siblings is not self-contained;
// v1 rejects it with the dependency named rather than emitting a broken move.
func TestMoveDeclLocalDepsReject(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"UseFetch","to_pkg":"demo/lib"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if !strings.Contains(rej.Reason+rej.Detail, "Fetch") && rej.Reason != "patch does not typecheck" {
		t.Fatalf("reject must name the local dependency or fail typecheck: %v %v", rej.Reason, rej.Detail)
	}
}
