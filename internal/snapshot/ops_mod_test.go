package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// toolsVersion pins add_dependency tests to the golang.org/x/tools version
// ago itself depends on: the local module cache necessarily holds it, so
// the go tool serves it without the network.
func toolsVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "golang.org/x/tools").Output()
	if err != nil {
		t.Skipf("cannot resolve x/tools version: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestPatchAddDependency(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	res, err := s.Patch([]byte(`{"ops":[{"op":"add_dependency","module":"golang.org/x/tools","version":"` + v + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	if !strings.Contains(string(b), "golang.org/x/tools") {
		t.Fatalf("require not added:\n%s", b)
	}
	if _, err := s.inspect("demo/lib", "Double"); err != nil {
		t.Errorf("snapshot broken after add: %v", err)
	}
}

// A later op's rejection restores go.mod and go.sum byte-for-byte.
func TestPatchAddDependencyRestoresOnLaterReject(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	orig, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	_, err := s.Patch([]byte(`{"ops":[{"op":"add_dependency","module":"golang.org/x/tools","version":"` + v + `"},
		{"op":"upsert_decl","pkg":"demo/lib","text":"func Broken() int {\n\treturn undefinedIdent\n}"}]}`))
	if err == nil {
		t.Fatal("want rejection")
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	if string(after) != string(orig) {
		t.Fatalf("go.mod not restored:\n%s", after)
	}
}

func TestPatchRemoveDependencyRejectsWhileImported(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	if _, err := s.Patch([]byte(`{"ops":[{"op":"add_dependency","module":"golang.org/x/tools","version":"` + v + `"}]}`)); err != nil {
		t.Fatal(err)
	}
	use := "package lib\n\nimport \"golang.org/x/tools/txtar\"\n\nfunc Arc() string {\n\treturn txtar.Format(&txtar.Archive{})\n}\n"
	if err := os.WriteFile(filepath.Join(s.dir, "lib", "use.go"), []byte(use), 0o644); err != nil {
		t.Fatal(err)
	}
	s.loaded = false
	_, err := s.Patch([]byte(`{"ops":[{"op":"remove_dependency","module":"golang.org/x/tools"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "module is still imported" {
		t.Fatalf("got %v", err)
	}
	if len(rej.Diagnostics) == 0 {
		t.Fatal("rejection must list importing positions")
	}
}

func TestPatchRemoveDependencyDropsUnimported(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	if _, err := s.Patch([]byte(`{"ops":[{"op":"add_dependency","module":"golang.org/x/tools","version":"` + v + `"}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"ops":[{"op":"remove_dependency","module":"golang.org/x/tools"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	if strings.Contains(string(b), "golang.org/x/tools") {
		t.Fatalf("require not dropped:\n%s", b)
	}
}

// mod_tidy drops a require nothing imports.
func TestPatchModTidy(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	cmd := exec.Command("go", "mod", "edit", "-require=golang.org/x/tools@"+v)
	cmd.Dir = s.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed require: %v\n%s", err, out)
	}
	res, err := s.Patch([]byte(`{"ops":[{"op":"mod_tidy"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	if strings.Contains(string(b), "golang.org/x/tools") {
		t.Fatalf("tidy kept the unused require:\n%s", b)
	}
}

func TestPatchModTidyDryRun(t *testing.T) {
	s := demo(t)
	v := toolsVersion(t)
	cmd := exec.Command("go", "mod", "edit", "-require=golang.org/x/tools@"+v)
	cmd.Dir = s.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed require: %v\n%s", err, out)
	}
	orig, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	res, err := s.Patch([]byte(`{"dry_run":true,"ops":[{"op":"mod_tidy"}]}`))
	if err != nil || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "go.mod"))
	if string(after) != string(orig) {
		t.Fatal("dry_run changed go.mod")
	}
}
