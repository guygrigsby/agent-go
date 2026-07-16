package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRepo builds a two-commit git repo: parent declares the functions,
// head is the ground-truth add-param commit.
func fixtureRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "init", "-q")
	write("go.mod", "module example.com/fix\n\ngo 1.22\n")
	write("lib.go", `package fix

func Greet(name string) string { return "hi " + name }

func (s *Server) Handle(path string) error { return nil }

type Server struct{}
`)
	run("git", "add", "-A")
	run("git", "commit", "-qm", "base")
	write("lib.go", `package fix

import "context"

func Greet(name string, loud bool) string { return "hi " + name }

func (s *Server) Handle(ctx context.Context, path string) error { return nil }

type Server struct{}
`)
	run("git", "add", "-A")
	run("git", "commit", "-qm", "add params")
	return repo, run("git", "rev-parse", "HEAD")
}

func TestExtractAddParams(t *testing.T) {
	repo, sha := fixtureRepo(t)
	specs, note := extractAddParams(repo, sha)
	if note != "" {
		t.Fatalf("unexpected review note: %s", note)
	}
	want := map[string]string{
		"Greet|loud":        "bool",
		"Server.Handle|ctx": "context.Context",
	}
	if len(specs) != len(want) {
		t.Fatalf("want %d specs, got %+v", len(want), specs)
	}
	for _, s := range specs {
		if s.Pkg != "example.com/fix" {
			t.Fatalf("pkg: %+v", s)
		}
		typ, ok := want[s.Sym+"|"+s.Name]
		if !ok || typ != s.Type {
			t.Fatalf("unexpected spec %+v", s)
		}
	}
}
