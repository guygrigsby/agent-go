package main

import (
	"fmt"
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

// gitFixture builds a two-commit repo: before at the parent, after at
// HEAD, module example.com/mv.
func gitFixture(t *testing.T, before, after map[string]string, subject string) (repo, sha string) {
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
	for name, content := range before {
		write(name, content)
	}
	run("git", "add", "-A")
	run("git", "commit", "-qm", "base")
	for name, content := range after {
		write(name, content)
	}
	run("git", "add", "-A")
	run("git", "commit", "-qm", subject)
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

// A same-named declaration in an unrelated changed package must not eat
// the move: pairing is per name per directory, not first file wins.
func TestExtractMovesSurvivesNameCollision(t *testing.T) {
	repo, sha := gitFixture(t, map[string]string{
		"a/a.go":    "package a\n\nfunc Helper() int { return 1 }\n",
		"b/b.go":    "package b\n",
		"0col/c.go": "package col\n\nfunc Helper() int { return 9 }\n\nfunc Touched() int { return 0 }\n",
	}, map[string]string{
		"a/a.go":    "package a\n",
		"b/b.go":    "package b\n\nfunc Helper() int { return 1 }\n",
		"0col/c.go": "package col\n\nfunc Helper() int { return 9 }\n\nfunc Touched() int { return 1 }\n",
	}, "Move Helper into b")
	specs, note := extractMoves(repo, sha)
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	if len(specs) != 1 || specs[0].Sym != "Helper" ||
		specs[0].Pkg != "example.com/mv/a" || specs[0].ToPkg != "example.com/mv/b" {
		t.Fatalf("collision ate the move: %+v", specs)
	}
}

// A declaration moved AND renamed (vault 45b0179a: mergeStates became
// MergeReplicationStates on its way to api) pairs by body fingerprint
// with every renamed sibling normalized out, and records to_name.
func TestExtractMovesFindsRenamedMove(t *testing.T) {
	repo, sha := gitFixture(t, map[string]string{
		"a/util.go": "package a\n\nfunc compareStates(s1, s2 string) int {\n\treturn len(s1) - len(s2)\n}\n\nfunc mergeStates(old []string, s string) []string {\n\tif compareStates(s, s) == 0 {\n\t\treturn append(old, s)\n\t}\n\treturn old\n}\n",
		"b/b.go":    "package b\n",
	}, map[string]string{
		"a/util.go": "package a\n",
		"b/b.go":    "package b\n\nfunc compareReplicationStates(s1, s2 string) int {\n\treturn len(s1) - len(s2)\n}\n\nfunc MergeReplicationStates(old []string, s string) []string {\n\tif compareReplicationStates(s, s) == 0 {\n\t\treturn append(old, s)\n\t}\n\treturn old\n}\n",
	}, "Move mergeStates utils to b")
	specs, note := extractMoves(repo, sha)
	if note != "" {
		t.Fatalf("unexpected note: %s", note)
	}
	if len(specs) != 2 {
		t.Fatalf("want 2 compound specs, got %+v", specs)
	}
	byOld := map[string]MoveSpec{}
	for _, s := range specs {
		byOld[s.Sym] = s
	}
	m := byOld["mergeStates"]
	if m.ToPkg != "example.com/mv/b" || m.ToName != "MergeReplicationStates" {
		t.Fatalf("compound spec wrong: %+v", m)
	}
	if c := byOld["compareStates"]; c.ToName != "compareReplicationStates" {
		t.Fatalf("sibling compound spec wrong: %+v", c)
	}
}

// Whole-package relocations (boundary b58dada4 moved 130 files) are out
// of task scope: reject with the count named, never emit a 50-spec task.
func TestExtractMovesRejectsWholePackageMove(t *testing.T) {
	before := map[string]string{"b/b.go": "package b\n"}
	after := map[string]string{"b/b.go": "package b\n"}
	var sb, sa strings.Builder
	sb.WriteString("package a\n")
	sa.WriteString("package b2\n")
	for i := range 12 {
		fmt.Fprintf(&sb, "\nfunc F%d() int { return %d }\n", i, i)
		fmt.Fprintf(&sa, "\nfunc F%d() int { return %d }\n", i, i)
	}
	before["a/a.go"] = sb.String()
	after["a/a.go"] = "package a\n"
	before["b2/b2.go"] = "package b2\n"
	after["b2/b2.go"] = sa.String()
	repo, sha := gitFixture(t, before, after, "Move package a to b2")
	specs, note := extractMoves(repo, sha)
	if len(specs) != 0 || !strings.Contains(note, "whole package") {
		t.Fatalf("want whole-package reject, got %d specs, note %q", len(specs), note)
	}
}
