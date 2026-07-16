package bench

// Manifest is one bench task, shared by the prep tool that writes it and
// the runner that scores it. Kind selects the goal predicate; empty means
// rename, the original task family.
type Manifest struct {
	Repo        string       `json:"repo"`
	SHA         string       `json:"sha"`
	Kind        string       `json:"kind,omitempty"`
	Prompt      string       `json:"prompt"`
	GoFiles     int          `json:"go_files,omitempty"`
	Renames     []RenameSpec `json:"renames,omitempty"`
	NeedsReview string       `json:"needs_review,omitempty"`
}

type RenameSpec struct {
	Pkg string `json:"pkg"`
	Sym string `json:"sym"`
	To  string `json:"to"`
}
