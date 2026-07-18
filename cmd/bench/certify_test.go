package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/agent-go/bench"
)

// certify sets flags from passing oracle evidence, and REVOKES them for
// tasks whose specs cannot verify (empty pkg: cobra_0960ff7f certified on
// vacuous all-zero evidence) or whose oracle evidence in the given runs
// uniformly fails.
func TestCertifySetsAndRevokes(t *testing.T) {
	dir := t.TempDir()
	writeEp := func(task string, pass bool) {
		ep := filepath.Join(dir, "run", task, "oracle", "0")
		if err := os.MkdirAll(ep, 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"task": task, "mode": "oracle", "pass": pass})
		if err := os.WriteFile(filepath.Join(ep, "episode.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeEp("demo_aaaaaaaa", true)
	writeEp("demo_cccccccc", false)
	tasks := []bench.Manifest{
		{Repo: "demo", SHA: "aaaaaaaa11111111", Kind: "rename",
			Renames: []bench.RenameSpec{{Pkg: "m/a", Sym: "X", To: "Y"}}},
		{Repo: "demo", SHA: "bbbbbbbb11111111", Kind: "rename", Certified: true,
			Renames: []bench.RenameSpec{{Pkg: "", Sym: "X", To: "Y"}}},
		{Repo: "demo", SHA: "cccccccc11111111", Kind: "rename", Certified: true,
			Renames: []bench.RenameSpec{{Pkg: "m/c", Sym: "X", To: "Y"}}},
	}
	tf := filepath.Join(dir, "tasks.json")
	raw, _ := json.MarshalIndent(tasks, "", "  ")
	if err := os.WriteFile(tf, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := certify([]string{filepath.Join(dir, "run")}, tf); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(tf)
	var got []bench.Manifest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	bySHA := map[string]bench.Manifest{}
	for _, m := range got {
		bySHA[m.SHA[:8]] = m
	}
	if !bySHA["aaaaaaaa"].Certified {
		t.Error("passing oracle evidence did not certify")
	}
	if bySHA["bbbbbbbb"].Certified {
		t.Error("invalid-spec task kept its certification")
	}
	if bySHA["cccccccc"].Certified {
		t.Error("uniformly failing oracle evidence kept its certification")
	}
}
