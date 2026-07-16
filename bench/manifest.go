package bench

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
