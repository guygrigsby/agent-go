package snapshot

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Every op in the live help catalog must have a row in the surface doc,
// and every op the doc marks shipped must exist in the catalog. The doc
// is the running list; this keeps it from rotting.
func TestSurfaceDocCurrent(t *testing.T) {
	raw, err := os.ReadFile("../../docs/specs/surface.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	shipped := map[string]bool{}
	for _, line := range strings.Split(doc, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 2 && strings.TrimSpace(fields[2]) == "shipped" {
			shipped[strings.Trim(strings.TrimSpace(fields[1]), "`")] = true
		}
	}
	s := demo(t)
	res, err := s.Help()
	if err != nil {
		t.Fatal(err)
	}
	rawOps, err := json.Marshal(res["ops"])
	if err != nil {
		t.Fatal(err)
	}
	var ops []struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(rawOps, &ops); err != nil || len(ops) == 0 {
		t.Fatalf("help catalog empty or unexpected shape: %v", err)
	}
	catalog := map[string]bool{}
	for _, entry := range ops {
		catalog[entry.Op] = true
		if !shipped[entry.Op] {
			t.Errorf("op %q is in the help catalog but not marked shipped in docs/specs/surface.md", entry.Op)
		}
	}
	for name := range shipped {
		if strings.Contains(name, " ") { // e.g. "wrap_stmts with:go" candidates never match
			continue
		}
		if !catalog[name] && !isQueryKindOrTool(name) {
			t.Errorf("surface.md marks %q shipped but the catalog does not have it", name)
		}
	}
}

// isQueryKindOrTool filters surface rows that are not patch ops.
func isQueryKindOrTool(name string) bool {
	switch name {
	case "status", "help", "query", "view", "patch", "test",
		"search", "inspect", "refs", "callers", "callees", "implementations", "doc":
		return true
	}
	return false
}
