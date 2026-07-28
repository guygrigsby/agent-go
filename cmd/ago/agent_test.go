package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/agent-go/internal/agent"
)

func TestAgentToolDefsCoverSurfacePlusFileTools(t *testing.T) {
	defs := agentToolDefs("semantic")
	got := map[string]bool{}
	for _, d := range defs {
		if d.Description == "" || d.Schema == nil {
			t.Fatalf("def %s missing description or schema", d.Name)
		}
		got[d.Name] = true
	}
	for _, name := range append(append([]string{}, realToolNames...), "read_file", "write_file") {
		if !got[name] {
			t.Fatalf("missing tool def %s; have %v", name, got)
		}
	}
	if len(defs) != len(realToolNames)+2 {
		t.Fatalf("unexpected extra defs: %v", got)
	}
}

func TestAgentToolsRouteFileToolsLocally(t *testing.T) {
	dir := t.TempDir()
	tools := newAgentTools(dir, "semantic")
	if out, isErr := tools.Call("write_file", map[string]any{"path": "notes.md", "content": "hi"}); isErr {
		t.Fatalf("write_file rejected: %s", out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "notes.md")); err != nil || string(b) != "hi" {
		t.Fatalf("file not written: %v %q", err, b)
	}
	if out, isErr := tools.Call("write_file", map[string]any{"path": "sneaky.go", "content": "package x"}); !isErr {
		t.Fatalf(".go write accepted: %s", out)
	}
}

func TestLoadAgentProfile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".ago"), 0o755)
	os.WriteFile(filepath.Join(dir, ".ago", "agent.json"), []byte(`{
		"default": "qwen",
		"profiles": [
			{"name": "qwen", "endpoint": "https://llama.lab.aeryx.ai/v1", "model": "qwen",
			 "sampler": {"temperature": 0.7}},
			{"name": "glm", "endpoint": "https://llama.lab.aeryx.ai/v1", "model": "glm"}
		]}`), 0o644)

	p, err := loadAgentProfile(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "qwen" || p.Sampler["temperature"] != 0.7 {
		t.Fatalf("default profile = %+v", p)
	}
	p, err = loadAgentProfile(dir, "glm", "", "")
	if err != nil || p.Model != "glm" {
		t.Fatalf("named profile = %+v err=%v", p, err)
	}
	p, err = loadAgentProfile(dir, "", "http://other/v1", "mini")
	if err != nil || p.Endpoint != "http://other/v1" || p.Model != "mini" {
		t.Fatalf("flag override = %+v err=%v", p, err)
	}
	if _, err := loadAgentProfile(dir, "nope", "", ""); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestAgentSurfaces(t *testing.T) {
	names := func(defs []agent.ToolDef) map[string]bool {
		m := map[string]bool{}
		for _, d := range defs {
			m[d.Name] = true
		}
		return m
	}
	raw := names(agentToolDefs("raw"))
	want := map[string]bool{"read_file": true, "write_file": true, "test": true}
	if !maps.Equal(raw, want) {
		t.Fatalf("raw surface drifted from ADR 0006: %v", raw)
	}
	sem := names(agentToolDefs("semantic"))
	if !sem["upsert_decl"] || !sem["read_file"] || !sem["write_file"] {
		t.Fatalf("semantic surface lost a tool: %v", sem)
	}
	if sem["shell"] || sem["bash"] {
		t.Fatalf("no shell on any surface: %v", sem)
	}
}

func TestRawSurfaceGatesDispatch(t *testing.T) {
	tools := newAgentTools(t.TempDir(), "raw")
	out, isErr := tools.Call("rename", map[string]any{"pkg": "x", "sym": "A", "to": "B"})
	if !isErr || !strings.Contains(out, "raw surface") {
		t.Fatalf("raw dispatch must reject op calls by name: %q %v", out, isErr)
	}
	if out, isErr := tools.Call("write_file", map[string]any{"path": "a.go", "content": "package a\n"}); isErr {
		t.Fatalf("raw write_file must land .go: %s", out)
	}
}

func TestLoadAgentProfileWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadAgentProfile(dir, "", "", ""); err == nil {
		t.Fatal("missing config with no flags must error")
	}
	p, err := loadAgentProfile(dir, "", "http://box/v1", "m")
	if err != nil || p.Endpoint != "http://box/v1" || p.Model != "m" {
		t.Fatalf("flag-only profile = %+v err=%v", p, err)
	}
}
