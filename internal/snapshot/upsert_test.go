package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertAddFunc(t *testing.T) {
	s := demo(t)
	res, err := s.UpsertDecl("demo/lib", "// Triple triples v.\nfunc Triple(v int) int {\n\treturn v * 3\n}")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["action"] != "added" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.Inspect("demo/lib", "Triple"); err != nil {
		t.Errorf("new decl not queryable: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	if !strings.Contains(string(b), "Triple triples v") {
		t.Errorf("agent.go missing decl:\n%s", b)
	}
}

func TestUpsertAutoImport(t *testing.T) {
	s := demo(t)
	res, err := s.UpsertDecl("demo/lib", "func Describe(v int) string {\n\treturn fmt.Sprintf(\"v=%d\", v)\n}")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "agent.go"))
	if !strings.Contains(string(b), `"fmt"`) {
		t.Errorf("import not added:\n%s", b)
	}
}

func TestUpsertReplace(t *testing.T) {
	s := demo(t)
	res, err := s.UpsertDecl("demo/lib", "func Double(v int) int {\n\treturn v << 1\n}")
	if err != nil {
		t.Fatal(err)
	}
	if res["action"] != "replaced" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), "v << 1") || strings.Contains(string(b), "v * 2") {
		t.Errorf("declaration not replaced in place:\n%s", b)
	}
	refs, err := s.Refs("demo/lib", "Double", 0)
	if err != nil || refs["count"].(int) != 2 {
		t.Errorf("refs after replace: %v %v", refs, err)
	}
}

func TestUpsertRejectBadType(t *testing.T) {
	s := demo(t)
	_, err := s.UpsertDecl("demo/lib", "func Bad() Nope {\n\treturn Nope{}\n}")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "declaration does not typecheck" {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "lib", "agent.go")); err == nil {
		t.Error("rejected upsert left agent.go behind")
	}
}

func TestUpsertNewPackage(t *testing.T) {
	s := demo(t)
	res, err := s.UpsertDecl("demo/util", "func Clamp(v, lo, hi int) int {\n\tif v < lo {\n\t\treturn lo\n\t}\n\tif v > hi {\n\t\treturn hi\n\t}\n\treturn v\n}")
	if err != nil {
		t.Fatal(err)
	}
	if res["action"] != "created-package" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.Inspect("demo/util", "Clamp"); err != nil {
		t.Errorf("new package symbol not queryable: %v", err)
	}
}
