package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The MCP tool surface must be exactly the help catalog's tools plus the
// four sugar ops: a tool added on one side without the other leaves either
// the wire or the documentation lying.
func TestMCPSurfaceMatchesHelpCatalog(t *testing.T) {
	mcpNames := map[string]bool{}
	for _, tool := range mcpTools() {
		mcpNames[tool.Name] = true
	}
	sugar := []string{"rename", "set_body", "add_param", "upsert_decl"}
	// The help catalog's tool list arrives over the same wire agents use.
	abs := startMCPFixture(t, "guardmod")
	text, isErr := mcpCall(abs, "help", nil)
	if isErr {
		t.Fatalf("help failed: %s", text)
	}
	var help struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(text), &help); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, tool := range help.Tools {
		want[tool.Name] = true
	}
	for _, s := range sugar {
		want[s] = true
	}
	for name := range want {
		if !mcpNames[name] {
			t.Errorf("help documents %q but MCP does not serve it", name)
		}
	}
	for name := range mcpNames {
		if !want[name] {
			t.Errorf("MCP serves %q but the help catalog does not document it", name)
		}
	}
}

// Every ago_* tool name the init scaffold teaches must resolve to a real
// tool through the alias layer; teaching a dead name poisons every agent
// session started from the scaffold.
func TestInitTeachingNamesResolve(t *testing.T) {
	src, err := os.ReadFile("init.go")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`ago_[a-z_]+`).FindAllString(string(src), -1)
	if len(names) < 5 {
		t.Fatalf("suspiciously few ago_* mentions in init.go: %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if canonical, _ := resolveToolName(n); canonical == "" {
			t.Errorf("init.go teaches %q, which resolves to no tool", n)
		}
	}
}

// README's statement-op count is computed, not trusted: it must equal the
// number of rows in language.md's statement-op table.
func TestReadmeStatementOpCount(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(\d+) statement ops`).FindStringSubmatch(string(readme))
	if m == nil {
		t.Skip("README no longer states a statement-op count")
	}
	lang, err := os.ReadFile("../../docs/specs/language.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(lang)
	start := strings.Index(section, "### Statement ops")
	end := strings.Index(section[start:], "### Test ops")
	rows := 0
	for _, line := range strings.Split(section[start:start+end], "\n") {
		if strings.HasPrefix(line, "| `") {
			rows++
		}
	}
	if m[1] != strconv.Itoa(rows) {
		t.Errorf("README says %s statement ops; language.md's table has %d rows", m[1], rows)
	}
}
