package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// moveFixture: parent declares Helper in pkg a; head has it in pkg b and
// not in a. A second decl stays put as noise.
func moveFixture(t *testing.T) (repo, sha string) {
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
		p := filepath.Join(repo, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "init", "-q")
	write("go.mod", "module example.com/mv\n\ngo 1.22\n")
	write("a/a.go", "package a\n\nfunc Helper() int { return 1 }\n\nfunc Stays() int { return 2 }\n")
	write("b/b.go", "package b\n")
	run("git", "add", "-A")
	run("git", "commit", "-qm", "base")
	write("a/a.go", "package a\n\nfunc Stays() int { return 2 }\n")
	write("b/b.go", "package b\n\nfunc Helper() int { return 1 }\n")
	run("git", "add", "-A")
	run("git", "commit", "-qm", "Move Helper into b")
	return repo, run("git", "rev-parse", "HEAD")
}

func TestExtractMoves(t *testing.T) {
	repo, sha := moveFixture(t)
	specs, note := extractMoves(repo, sha)
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %+v", specs)
	}
	m := specs[0]
	if m.Sym != "Helper" || m.Pkg != "example.com/mv/a" || m.ToPkg != "example.com/mv/b" {
		t.Fatalf("wrong spec: %+v", m)
	}
}
