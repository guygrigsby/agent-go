package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRotImporter writes a package importing demo/lib that carries its own
// type error, before the snapshot's first load. A broken reverse importer
// lands in every demo/lib mutation's affected set — the shape of unrelated
// rot mined-repo parent commits carry — so these tests prove the baseline
// contract: pre-existing diagnostics never block a mutation, only NEW ones
// reject.
func seedRotImporter(t *testing.T, s *Snapshot) {
	t.Helper()
	dir := filepath.Join(s.dir, "rotimp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package rotimp\n\nimport \"demo/lib\"\n\nvar _ = lib.Limit\n\nfunc Broken() int { return undefinedThing }\n"
	if err := os.WriteFile(filepath.Join(dir, "rotimp.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A rename in demo/lib must succeed even though a reverse importer carries a
// pre-existing type error: the rot is in the dirty set, but the rename adds
// no NEW diagnostics. The tolerated rot stays visible via pre_existing.
func TestRenameToleratesPreexistingRot(t *testing.T) {
	s := demo(t)
	seedRotImporter(t, s)
	res, err := s.Rename("demo/lib", "Double", "Twice")
	if err != nil {
		t.Fatalf("rename blocked by pre-existing rot in a reverse importer: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if res["pre_existing"] == nil {
		t.Errorf("want pre_existing set, got %v", res["pre_existing"])
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "func Twice") {
		t.Errorf("rename not written:\n%s", b)
	}
}

// A mutation that introduces a genuinely NEW error still rejects, and the
// reject lists only the new diagnostics — never the pre-existing rot.
func TestRenameNewErrorListsOnlyNewDiagnostics(t *testing.T) {
	s := demo(t)
	seedRotImporter(t, s)
	_, err := s.Rename("demo/lib", "Double", "Tail") // collides with existing Tail
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "rename does not typecheck" {
		t.Fatalf("want typecheck reject, got %v", err)
	}
	if len(rej.Diagnostics) == 0 {
		t.Fatal("reject carries no diagnostics")
	}
	for _, d := range rej.Diagnostics {
		if strings.Contains(d.Msg, "undefinedThing") {
			t.Errorf("pre-existing rot listed in reject: %v", d)
		}
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "func Double") {
		t.Errorf("failed rename not rolled back:\n%s", b)
	}
}

// The composable patch pipeline (delete_decl here) is equally tolerant: op
// baselines accumulate in patchCtx and filter once at end-of-list.
func TestPatchDeleteDeclToleratesPreexistingRot(t *testing.T) {
	s := demo(t)
	seedRotImporter(t, s)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","ops":[{"op":"delete_decl"}]}`))
	if err != nil {
		t.Fatalf("delete_decl blocked by pre-existing rot: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if res["pre_existing"] == nil {
		t.Errorf("want pre_existing set, got %v", res["pre_existing"])
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "lib", "use.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "func UseHelper") {
		t.Errorf("delete_decl did not remove the declaration:\n%s", b)
	}
}

// A patch introducing a NEW error rejects listing only the new diagnostics,
// even when the dirty set (affected reverse importers of the envelope pkg)
// carries pre-existing rot.
func TestPatchNewErrorListsOnlyNewDiagnostics(t *testing.T) {
	s := demo(t)
	seedRotImporter(t, s)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double","ops":[{"op":"set_body","body":"return undefinedNew"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" {
		t.Fatalf("want typecheck reject, got %v", err)
	}
	sawNew := false
	for _, d := range rej.Diagnostics {
		if strings.Contains(d.Msg, "undefinedThing") {
			t.Errorf("pre-existing rot listed in reject: %v", d)
		}
		if strings.Contains(d.Msg, "undefinedNew") {
			sawNew = true
		}
	}
	if !sawNew {
		t.Errorf("new diagnostic missing from reject: %v", rej.Diagnostics)
	}
}
