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
