package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddParam(t *testing.T) {
	s := demo(t)
	res, err := s.AddParam("demo/lib", "Double", "scale", "int", "1")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["callers_updated"].(int) != 1 {
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
	if _, err := s.Inspect("demo/lib", "Double"); err != nil {
		t.Errorf("post-accept inspect: %v", err)
	}
}

func TestAddParamMethod(t *testing.T) {
	s := demo(t)
	res, err := s.AddParam("demo/lib", "Store.Put", "clamp", "bool", "false")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["callers_updated"].(int) != 1 {
		t.Fatalf("got %v", res)
	}
	main, _ := os.ReadFile(filepath.Join(s.dir, "main.go"))
	if !strings.Contains(string(main), ", false)") {
		t.Errorf("method caller not updated:\n%s", main)
	}
}

func TestAddParamNeedsDefault(t *testing.T) {
	s := demo(t)
	_, err := s.AddParam("demo/lib", "Double", "scale", "int", "")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "default expression required" {
		t.Fatalf("got %v", err)
	}
}

func TestAddParamRejectsValueUse(t *testing.T) {
	s := demo(t)
	valueUse := filepath.Join(s.dir, "lib", "value.go")
	if err := os.WriteFile(valueUse, []byte("package lib\n\nvar Fn = Double\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddParam("demo/lib", "Double", "scale", "int", "1")
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason, "used as a value") || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
	lib, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(lib), "func Double(v int) int") {
		t.Errorf("rejected add-param mutated the declaration:\n%s", lib)
	}
}

func TestAddParamBadDefaultRejected(t *testing.T) {
	s := demo(t)
	_, err := s.AddParam("demo/lib", "Double", "scale", "int", `"nope"`)
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "add_param does not typecheck" || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
	main, _ := os.ReadFile(filepath.Join(s.dir, "main.go"))
	if strings.Contains(string(main), `"nope"`) {
		t.Errorf("rejected add-param left caller edit behind:\n%s", main)
	}
}

// writeScaleFixture drops a function whose body declares a local at the top
// level of the body — the shape behind agent-go-qus, where boundary's
// CreateNewAccountCli declares `ctx := context.Background()`. Parameters
// share the body block's scope, so naively adding a same-named parameter
// turns that := into "no new variables on left side of :=".
func writeScaleFixture(t *testing.T, s *Snapshot) string {
	t.Helper()
	scale := filepath.Join(s.dir, "lib", "scale.go")
	src := "package lib\n\nfunc Scale(v int) int {\n\tfactor := 2\n\treturn v * factor\n}\n"
	if err := os.WriteFile(scale, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return scale
}

func TestAddParamPromotesMatchingLocal(t *testing.T) {
	s := demo(t)
	scale := writeScaleFixture(t, s)
	use := filepath.Join(s.dir, "scale_use.go")
	if err := os.WriteFile(use, []byte("package main\n\nimport \"demo/lib\"\n\nvar scaled = lib.Scale(3)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.AddParam("demo/lib", "Scale", "factor", "int", "2")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["callers_updated"].(int) != 1 {
		t.Fatalf("got %v", res)
	}
	src, _ := os.ReadFile(scale)
	if !strings.Contains(string(src), "func Scale(v int, factor int) int") {
		t.Errorf("declaration not updated:\n%s", src)
	}
	if strings.Contains(string(src), "factor :=") {
		t.Errorf("superseded local declaration not removed:\n%s", src)
	}
	caller, _ := os.ReadFile(use)
	if !strings.Contains(string(caller), "lib.Scale(3, 2)") {
		t.Errorf("caller not updated:\n%s", caller)
	}
}

// TestAddParamMultiAssignRedeclareAccepted covers the boundary b4b95e0f
// shape: the body's top level has `ctx, cancelFunc := ...`. A multi-LHS :=
// that introduces at least one other new variable is legal Go after the
// parameter lands (the := redeclares the parameter, assigning it), so it is
// not a collision and must not reject.
func TestAddParamMultiAssignRedeclareAccepted(t *testing.T) {
	s := demo(t)
	pair := filepath.Join(s.dir, "lib", "pair.go")
	src := "package lib\n\nfunc Pair(v int) int {\n\tfactor, twice := v, v*2\n\t_ = twice\n\treturn v * factor\n}\n"
	if err := os.WriteFile(pair, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.AddParam("demo/lib", "Pair", "factor", "int", "2")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, _ := os.ReadFile(pair)
	if !strings.Contains(string(out), "func Pair(v int, factor int) int") {
		t.Errorf("declaration not updated:\n%s", out)
	}
	if !strings.Contains(string(out), "factor, twice := v, v*2") {
		t.Errorf("multi-LHS := body statement must survive untouched:\n%s", out)
	}
}

func TestAddParamLocalCollisionRejected(t *testing.T) {
	s := demo(t)
	scale := writeScaleFixture(t, s)
	_, err := s.AddParam("demo/lib", "Scale", "factor", "int", "3")
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason, "collides") || len(rej.Diagnostics) == 0 {
		t.Fatalf("got %v", err)
	}
	src, _ := os.ReadFile(scale)
	if !strings.Contains(string(src), "func Scale(v int) int") || !strings.Contains(string(src), "factor := 2") {
		t.Errorf("rejected add_param mutated the file:\n%s", src)
	}
}
