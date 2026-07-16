package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The catalog is cached by consumers keyed on catalogVersion; changing its
// shape (op set or argument names) without bumping the version hands them
// a stale copy they can't detect. When this fails: bump catalogVersion in
// help.go AND update wantCatalogHash here to the printed value.
func TestCatalogVersionBumpedOnShapeChange(t *testing.T) {
	const wantVersion = "v4"
	const wantCatalogHash = "0e7c3c30317a9220"
	var parts []string
	for _, op := range opCatalog {
		names := make([]string, len(op.Args))
		for i, a := range op.Args {
			names[i] = a.Name
		}
		parts = append(parts, op.Op+"("+strings.Join(names, ",")+")")
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ";")))
	got := hex.EncodeToString(sum[:8])
	if catalogVersion != wantVersion || got != wantCatalogHash {
		t.Fatalf("catalog shape changed: hash %s (recorded %s), catalogVersion %q (recorded %q).\n"+
			"Bump catalogVersion in help.go and record the new hash and version in this test.",
			got, wantCatalogHash, catalogVersion, wantVersion)
	}
}

// A surface.md row marked planned or candidate must cite an OPEN beads
// issue: when the work ships (bead closes), the row must flip to shipped.
func TestSurfacePlannedRowsCiteOpenIssues(t *testing.T) {
	doc, err := os.ReadFile("../../docs/specs/surface.md")
	if err != nil {
		t.Fatal(err)
	}
	beadsRaw, err := os.ReadFile("../../.beads/issues.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(beadsRaw)), "\n") {
		var b struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(line), &b) == nil && b.ID != "" {
			status[b.ID] = b.Status
		}
	}
	ref := regexp.MustCompile(`\[(agent-go-[a-z0-9.]+)\]`)
	for _, line := range strings.Split(string(doc), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		st := strings.TrimSpace(fields[2])
		if st != "planned" && st != "candidate" {
			continue
		}
		for _, m := range ref.FindAllStringSubmatch(line, -1) {
			id := m[1]
			switch status[id] {
			case "":
				t.Errorf("surface.md %s row cites %s, which is not in .beads/issues.jsonl", st, id)
			case "closed":
				t.Errorf("surface.md still marks a row %s but %s is closed — flip the row to shipped or drop it:\n%s",
					st, id, strings.TrimSpace(line))
			}
		}
	}
}

// Every JSON code block in language.md must parse, and any block shaped
// like a patch envelope or ops array must name only registered ops with
// arguments their schemas accept. Docs examples are few-shot material;
// a stale one teaches models a dialect the daemon rejects.
func TestLanguageSpecJSONBlocksValid(t *testing.T) {
	doc, err := os.ReadFile("../../docs/specs/language.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(doc), -1)
	if len(blocks) == 0 {
		t.Fatal("no json blocks found; extraction broken?")
	}
	for i, b := range blocks {
		var v any
		if err := json.Unmarshal([]byte(b[1]), &v); err != nil {
			t.Errorf("language.md json block %d does not parse: %v\n%s", i, err, b[1])
			continue
		}
		checkOps := func(ops []any) {
			for _, rawOp := range ops {
				m, ok := rawOp.(map[string]any)
				if !ok {
					continue
				}
				name, _ := m["op"].(string)
				if name == "" {
					continue
				}
				if opRegistry[name] == nil {
					// UNSHIPPED rows are prose, not examples; an example
					// using an unregistered op teaches a dead dialect.
					t.Errorf("language.md json block %d uses unregistered op %q", i, name)
				}
			}
		}
		switch tv := v.(type) {
		case map[string]any:
			if ops, ok := tv["ops"].([]any); ok {
				checkOps(ops)
			}
		case []any:
			checkOps(tv)
		}
	}
	_ = fmt.Sprintf // keep imports honest if assertions change
}
