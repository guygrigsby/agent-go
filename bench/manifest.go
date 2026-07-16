package bench

import "sort"

// Manifest is one bench task, shared by the prep tool that writes it and
// the runner that scores it. Kind selects the goal predicate; empty means
// rename, the original task family.
type Manifest struct {
	Repo        string         `json:"repo"`
	SHA         string         `json:"sha"`
	Kind        string         `json:"kind,omitempty"`
	Prompt      string         `json:"prompt"`
	GoFiles     int            `json:"go_files,omitempty"`
	Renames     []RenameSpec   `json:"renames,omitempty"`
	AddParams   []AddParamSpec `json:"add_params,omitempty"`
	Moves       []MoveSpec     `json:"moves,omitempty"`
	NeedsReview string         `json:"needs_review,omitempty"`

	// Certified marks that the oracle has replayed this task's ground
	// truth through the protocol and reached green. Model rounds run only
	// certified tasks (tenet 2: prove the task is solvable first); the
	// flag is flipped by `bench certify <run dir>` from committed oracle
	// evidence, never by hand.
	Certified bool `json:"certified,omitempty"`
}

// EligibleForModel reports whether a model round may spend time on this
// task: it must carry specs and be oracle certified.
func (t Manifest) EligibleForModel() bool { return t.HasSpecs() && t.Certified }

// suiteTasks selects the run tier. "full" (or empty) passes everything
// through; "smoke" picks one model-eligible task per kind, smallest repo
// first (by GoFiles, then repo name for determinism), so first contact
// with a new model or harness change costs minutes, not a round.
func suiteTasks(tasks []Manifest, suite string) []Manifest {
	if suite != "smoke" {
		return tasks
	}
	best := map[string]Manifest{}
	for _, t := range tasks {
		if !t.EligibleForModel() {
			continue
		}
		cur, ok := best[t.Kind]
		if !ok || t.GoFiles < cur.GoFiles ||
			(t.GoFiles == cur.GoFiles && t.Repo < cur.Repo) {
			best[t.Kind] = t
		}
	}
	kinds := make([]string, 0, len(best))
	for k := range best {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]Manifest, 0, len(best))
	for _, k := range kinds {
		out = append(out, best[k])
	}
	return out
}

// MoveSpec is one declaration the ground-truth commit relocated: the goal
// predicate checks it resolves in ToPkg and no longer in Pkg.
type MoveSpec struct {
	Pkg   string `json:"pkg"`
	Sym   string `json:"sym"`
	ToPkg string `json:"to_pkg"`
}

type RenameSpec struct {
	Pkg string `json:"pkg"`
	Sym string `json:"sym"`
	To  string `json:"to"`
}

// AddParamSpec is one parameter the ground-truth commit added: the goal
// predicate checks the target's signature declares it.
type AddParamSpec struct {
	Pkg  string `json:"pkg"`
	Sym  string `json:"sym"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// HasSpecs reports whether the manifest carries the specs its kind scores;
// a task without them is unextracted and unrunnable.
func (m Manifest) HasSpecs() bool {
	switch m.Kind {
	case "", "rename":
		return len(m.Renames) > 0
	case "add-param":
		return len(m.AddParams) > 0
	case "move":
		return len(m.Moves) > 0
	}
	return false
}
