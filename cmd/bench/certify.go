package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/guygrigsby/agent-go/bench"
)

// certify flips the certified flag on task manifests from committed oracle
// evidence: any oracle-mode episode.json with pass=true in the given run
// dirs certifies its task. It also REVOKES: a certified task whose specs
// cannot verify (HasSpecs false, e.g. an empty pkg from a pre-modules
// commit passes every predicate vacuously) or whose oracle evidence in
// these runs uniformly fails loses the flag. The flag gates model rounds
// (tenet 2) and is never set by hand.
func certify(runDirs []string, tasksGlob string) error {
	passes := map[string]bool{}
	saw := map[string]bool{}
	for _, d := range runDirs {
		matches, _ := filepath.Glob(filepath.Join(d, "*", "oracle", "*", "episode.json"))
		for _, m := range matches {
			raw, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			var e struct {
				Task string `json:"task"`
				Mode string `json:"mode"`
				Pass bool   `json:"pass"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Mode == "oracle" {
				saw[e.Task] = true
				if e.Pass {
					passes[e.Task] = true
				}
			}
		}
	}
	if len(saw) == 0 {
		return fmt.Errorf("no oracle episodes under %v", runDirs)
	}
	files, err := filepath.Glob(tasksGlob)
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no task manifests match %s", tasksGlob)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var tasks []bench.Manifest
		if err := json.Unmarshal(raw, &tasks); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		changed := false
		for i, t := range tasks {
			if len(t.SHA) < 8 {
				continue
			}
			key := fmt.Sprintf("%s_%s", t.Repo, t.SHA[:8])
			switch {
			case t.Certified && !t.HasSpecs():
				tasks[i].Certified = false
				changed = true
				fmt.Printf("revoked %s: specs cannot verify (%s)\n", key, f)
			case t.Certified && saw[key] && !passes[key]:
				tasks[i].Certified = false
				changed = true
				fmt.Printf("revoked %s: oracle evidence fails (%s)\n", key, f)
			case passes[key] && !t.Certified && t.HasSpecs():
				tasks[i].Certified = true
				changed = true
				fmt.Printf("certified %s (%s)\n", key, f)
			}
		}
		if !changed {
			continue
		}
		out, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(f, append(out, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}
