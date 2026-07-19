package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileRejectsGoPaths(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTools(dir)
	for _, p := range []string{"main.go", "pkg/sub/thing.go", "weird/Path.GO"} {
		out, isErr := ft.Call("write_file", map[string]any{"path": p, "content": "package x"})
		if !isErr {
			t.Fatalf("write_file %s accepted: %s", p, out)
		}
		if !strings.Contains(out, "upsert_decl") {
			t.Fatalf("rejection must point at the op surface, got %s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s written despite rejection", p)
		}
	}
}

func TestWriteFileEscapesRejected(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTools(dir)
	for _, p := range []string{"../outside.txt", "/etc/motd", "a/../../up.txt"} {
		if out, isErr := ft.Call("write_file", map[string]any{"path": p, "content": "x"}); !isErr {
			t.Fatalf("escape %s accepted: %s", p, out)
		}
	}
}

func TestWriteThenReadNonGoFile(t *testing.T) {
	dir := t.TempDir()
	ft := NewFileTools(dir)
	out, isErr := ft.Call("write_file", map[string]any{"path": "docs/notes.md", "content": "hello"})
	if isErr {
		t.Fatalf("write_file rejected: %s", out)
	}
	got, isErr := ft.Call("read_file", map[string]any{"path": "docs/notes.md"})
	if isErr || got != "hello" {
		t.Fatalf("read_file = %q isErr=%v", got, isErr)
	}
}

func TestReadFileReadsGoFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	ft := NewFileTools(dir)
	got, isErr := ft.Call("read_file", map[string]any{"path": "main.go"})
	if isErr || got != "package main" {
		t.Fatalf("read_file = %q isErr=%v", got, isErr)
	}
}

func TestFileToolsUnknownName(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	if _, isErr := ft.Call("delete_file_raw", map[string]any{"path": "x"}); !isErr {
		t.Fatal("unknown tool accepted")
	}
}
