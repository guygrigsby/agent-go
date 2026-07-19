package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// FileTools serves read_file and write_file for one workspace root.
// write_file rejects .go paths mechanically: Go source is reachable only
// through validated ops, and the invariant lives here, not in the
// prompt (ADR 0005). Everything stays inside root; escapes reject.
type FileTools struct {
	root string
}

func NewFileTools(root string) *FileTools { return &FileTools{root: root} }

func (f *FileTools) Call(name string, args map[string]any) (string, bool) {
	rel, _ := args["path"].(string)
	abs, ok := f.resolve(rel)
	if !ok {
		return "path escapes the workspace: " + rel, true
	}
	switch name {
	case "read_file":
		b, err := os.ReadFile(abs)
		if err != nil {
			return err.Error(), true
		}
		return string(b), false
	case "write_file":
		if strings.EqualFold(filepath.Ext(rel), ".go") {
			return "write_file does not touch Go source; use upsert_decl and the other ago ops, which validate before landing", true
		}
		content, _ := args["content"].(string)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err.Error(), true
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err.Error(), true
		}
		return "wrote " + rel, false
	default:
		return "unknown file tool " + name, true
	}
}

func (f *FileTools) resolve(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	abs := filepath.Clean(filepath.Join(f.root, rel))
	if abs != f.root && !strings.HasPrefix(abs, f.root+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}
