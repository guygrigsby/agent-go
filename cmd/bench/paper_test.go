package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The paper's tables are generated from episode data, never hand-written
// (tenet 12 meets tenet 1): the generator turns run dirs into booktabs
// rows and summary macros the LaTeX inputs verbatim.
func TestPaperTablesGenerated(t *testing.T) {
	dir := writeRunDir(t)
	out := t.TempDir()
	if err := paperTables([]string{dir}, out); err != nil {
		t.Fatal(err)
	}
	table, err := os.ReadFile(filepath.Join(out, "results_table.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(table), "traefik\\_1") || !strings.Contains(string(table), "1/1") {
		t.Fatalf("table missing cells:\n%s", table)
	}
	macros, err := os.ReadFile(filepath.Join(out, "summary_macros.tex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{`\SemanticPassRate`, `\RawPassRate`, `\EpisodeCount`} {
		if !strings.Contains(string(macros), m) {
			t.Fatalf("macros missing %s:\n%s", m, macros)
		}
	}
}
