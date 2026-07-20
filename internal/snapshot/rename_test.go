package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRename(t *testing.T) {
	s := demo(t)
	res, err := s.Rename("demo/lib", "Double", "Twice")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["references"].(int) != 2 || len(res["files"].([]string)) != 2 {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "main.go"))
	if !strings.Contains(string(b), "lib.Twice(") {
		t.Errorf("caller not rewritten:\n%s", b)
	}
	if _, err := s.inspect("demo/lib", "Twice"); err != nil {
		t.Errorf("renamed symbol not queryable: %v", err)
	}
	// The daemon's own writes must not read as external edits.
	refs, err := s.refs("demo/lib", "Twice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if refs["load_ms"].(int64) != 0 {
		t.Error("query after accepted rename paid a full reload")
	}
}

func TestRenameMethod(t *testing.T) {
	s := demo(t)
	res, err := s.Rename("demo/lib", "Store.Put", "Set")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "main.go"))
	if !strings.Contains(string(b), "s.Set(") {
		t.Errorf("method call not rewritten:\n%s", b)
	}
}

func TestRenameCollision(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	_, err := s.Rename("demo/lib", "Double", "Limit")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "rename does not typecheck" {
		t.Fatalf("want typecheck reject, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if string(orig) != string(after) {
		t.Error("failed rename did not roll back")
	}
}

// helper -> h: at the use site inside UseHelper a local func-typed variable
// h shadows the new name with an identical signature, so the compiler is
// happy but the call silently rebinds. The resolution check must catch it.
func TestRenameCapture(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(filepath.Join(s.dir, "lib", "use.go"))
	_, err := s.Rename("demo/lib", "helper", "h")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "reference captured by another declaration" {
		t.Fatalf("want capture reject, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "lib", "use.go"))
	if string(orig) != string(after) {
		t.Error("failed rename did not roll back")
	}
}

func TestRenameBadIdentifier(t *testing.T) {
	s := demo(t)
	_, err := s.Rename("demo/lib", "Double", "not valid")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "new name is not a valid identifier" {
		t.Fatalf("got %v", err)
	}
}

// Go convention ties a doc comment's leading identifier to the decl name;
// rename must carry it (agent-go-1z3, found by the first driver dogfood
// episode: rename left "// Greet returns..." above func Hello).
func TestRenameCarriesDocCommentLeadingIdentifier(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "// Greet returns a greeting.\nfunc Greet(name string) string { return name }"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename("demo/lib", "Greet", "Hello"); err != nil {
		t.Fatal(err)
	}
	src := readLibFile(t, s, "Hello")
	if strings.Contains(src, "// Greet returns") {
		t.Fatalf("doc comment kept the old name:\n%s", src)
	}
	if !strings.Contains(src, "// Hello returns a greeting.") {
		t.Fatalf("doc comment not carried:\n%s", src)
	}
}

// A doc mentioning the old name beyond the leading identifier is prose;
// only the convention-bound leading word moves.
func TestRenameLeavesDocProseAlone(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "// Wave waves. Unlike Greet it is silent.\nfunc Wave() {}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDecl("demo/lib", "// Greet returns a greeting.\nfunc Greet(name string) string { return name }"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename("demo/lib", "Greet", "Hello"); err != nil {
		t.Fatal(err)
	}
	src := readLibFile(t, s, "Wave")
	if !strings.Contains(src, "Unlike Greet it is silent") {
		t.Fatalf("prose mention rewritten:\n%s", src)
	}
}

func readLibFile(t *testing.T, s *Snapshot, sym string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.dir, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(s.dir, "lib", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), sym+"(") {
			return string(b)
		}
	}
	t.Fatalf("no lib file declares %s", sym)
	return ""
}
