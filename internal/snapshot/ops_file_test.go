package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// delete_file rejects while the file declares symbols referenced from
// anywhere outside it; the rejection lists the reference positions so the
// caller knows what to move or delete first.
func TestPatchDeleteFileRejectsWhileReferenced(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"ops":[{"op":"delete_file","path":"lib/lib.go"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "file declares referenced symbols" {
		t.Fatalf("got %v", err)
	}
	if len(rej.Diagnostics) == 0 {
		t.Fatal("rejection must list reference positions")
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "lib.go")); serr != nil {
		t.Fatal("file was deleted despite rejection")
	}
}

func TestPatchDeleteFileRemovesUnreferenced(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Seed() int { return 0 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"ops":[{"op":"delete_file","path":"lib/agent.go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); serr == nil {
		t.Fatal("file still on disk")
	}
	if _, err := s.inspect("demo/lib", "Seed"); err == nil {
		t.Error("deleted symbol still queryable")
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("snapshot broken after delete: %v", err)
	}
}

// A later op's rejection restores the deleted file byte-for-byte.
func TestPatchDeleteFileRestoresOnLaterReject(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Seed() int { return 0 }"); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	_, err := s.Patch([]byte(`{"ops":[{"op":"delete_file","path":"lib/agent.go"},
		{"op":"upsert_decl","pkg":"demo/lib","text":"func Broken() int {\n\treturn undefinedIdent\n}"}]}`))
	if err == nil {
		t.Fatal("want rejection")
	}
	after, rerr := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	if rerr != nil || string(after) != string(orig) {
		t.Fatalf("file not restored: %v\n%s", rerr, after)
	}
	if _, err := s.inspect("demo/lib", "Seed"); err != nil {
		t.Errorf("snapshot broken after restore: %v", err)
	}
}

func TestPatchDeleteFileDryRun(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Seed() int { return 0 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"dry_run":true,
		"ops":[{"op":"delete_file","path":"lib/agent.go"}]}`))
	if err != nil || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); serr != nil {
		t.Fatal("dry_run deleted the file")
	}
}

// move_file within a package is a pure rename: same package, same
// content, new path.
func TestPatchMoveFileRenamesWithinPackage(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"ops":[{"op":"move_file","from":"lib/lib.go","to":"lib/core.go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "lib.go")); serr == nil {
		t.Fatal("old path still on disk")
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "core.go")); serr != nil {
		t.Fatal("new path missing")
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("symbol lost in rename: %v", err)
	}
}

// Cross-package move rewrites the package clause to the target package and
// drops a now-self import if one appears; the moved file's symbols join the
// target package.
func TestPatchMoveFileCrossPackage(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Seed() int { return 0 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"ops":[{"op":"move_file","from":"lib/agent.go","to":"sig/seed.go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, rerr := os.ReadFile(filepath.Join(s.dir, "sig", "seed.go"))
	if rerr != nil || !strings.Contains(string(b), "package sig") {
		t.Fatalf("package clause not rewritten: %v\n%s", rerr, b)
	}
	if _, err := s.inspect("demo/sig", "Seed"); err != nil {
		t.Errorf("moved symbol not in target package: %v", err)
	}
}

// Cross-package moves reject while the file declares externally referenced
// symbols (their qualifiers would all be wrong); a same-package rename never
// needs that check.
func TestPatchMoveFileCrossPackageRejectsWhileReferenced(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"ops":[{"op":"move_file","from":"lib/lib.go","to":"sig/lib.go"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "file declares referenced symbols" {
		t.Fatalf("got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "lib.go")); serr != nil {
		t.Fatal("file moved despite rejection")
	}
}

// A later op's rejection restores the move.
func TestPatchMoveFileRestoresOnLaterReject(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	_, err := s.Patch([]byte(`{"ops":[{"op":"move_file","from":"lib/lib.go","to":"lib/core.go"},
		{"op":"upsert_decl","pkg":"demo/lib","text":"func Broken() int {\n\treturn undefinedIdent\n}"}]}`))
	if err == nil {
		t.Fatal("want rejection")
	}
	after, rerr := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if rerr != nil || string(after) != string(orig) {
		t.Fatalf("original not restored: %v", rerr)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "core.go")); serr == nil {
		t.Fatal("moved copy survived a rejected patch")
	}
}

func TestPatchMoveFileDryRun(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"dry_run":true,
		"ops":[{"op":"move_file","from":"lib/lib.go","to":"lib/core.go"}]}`))
	if err != nil || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "lib.go")); serr != nil {
		t.Fatal("dry_run moved the file")
	}
	if _, serr := os.Stat(filepath.Join(s.dir, "lib", "core.go")); serr == nil {
		t.Fatal("dry_run left the moved copy")
	}
}

func TestPatchDeleteFileUnknownPath(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"ops":[{"op":"delete_file","path":"lib/nope.go"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "file not found" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(rej.Detail, "lib/nope.go") {
		t.Fatalf("detail missing path: %v", rej.Detail)
	}
}
