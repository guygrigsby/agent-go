package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Tenet 2: model rounds spend time only on tasks the oracle has proven
// solvable. Specs alone are not enough.
func TestEligibleForModelRequiresCertification(t *testing.T) {
	m := Manifest{Repo: "cobra", SHA: "0960ff7f0b23e50d02b8c8bcb0ee2f9c2c6be1d1",
		Renames: []RenameSpec{{Pkg: "p", Sym: "A", To: "B"}}}
	if m.EligibleForModel() {
		t.Fatal("uncertified task must not be model-eligible")
	}
	m.Certified = true
	if !m.EligibleForModel() {
		t.Fatal("certified task with specs must be model-eligible")
	}
	m.Renames = nil
	if m.EligibleForModel() {
		t.Fatal("certification without specs must not be model-eligible")
	}
}

// Tenet 7: every task is mined from a real commit, never hand-written.
// Each manifest entry must cite a full commit SHA from a known repo.
func TestTaskManifestsCiteRealCommits(t *testing.T) {
	known := map[string]bool{"traefik": true, "vault": true, "boundary": true, "cobra": true}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	paths, err := filepath.Glob("tasks-*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no task manifests: %v", err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var batch []Manifest
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, m := range batch {
			if !known[m.Repo] {
				t.Errorf("%s: unknown repo %q; add it here if a new source repo is deliberate", p, m.Repo)
			}
			if !sha.MatchString(m.SHA) {
				t.Errorf("%s: %s_%s does not cite a full commit sha", p, m.Repo, m.SHA)
			}
		}
	}
}

// Tenet 8: committed episode evidence must carry the identity axes that
// make runs distinguishable. A record that cannot say which profile, task,
// and mode produced it is an anecdote.
func TestCommittedEpisodesCarryIdentity(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("results", "*", "episodes.jsonl"))
	if len(paths) == 0 {
		t.Skip("no committed results")
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// Profiles shipped mid 2026-07-15; earlier evidence is grandfathered
		// (evidence is never rewritten), everything after must carry one.
		needsProfile := filepath.Base(filepath.Dir(p)) >= "20260715-12"
		for i, line := range splitLines(raw) {
			var e struct {
				Task    string `json:"task"`
				Mode    string `json:"mode"`
				Profile string `json:"profile"`
			}
			if err := json.Unmarshal(line, &e); err != nil {
				t.Errorf("%s line %d: %v", p, i+1, err)
				continue
			}
			if e.Task == "" || e.Mode == "" || (needsProfile && e.Profile == "") {
				t.Errorf("%s line %d: missing identity (task=%q mode=%q profile=%q)", p, i+1, e.Task, e.Mode, e.Profile)
			}
		}
	}
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				out = append(out, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}
