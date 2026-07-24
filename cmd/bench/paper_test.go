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
	// Per-model result numbers are generated too, so the abstract cites no
	// literals. Fixture: glm semantic 1/1 = 100%, raw 0/1 = 0%.
	for _, m := range []string{`\GlmSemanticPass{100\%}`, `\GlmRawPass{0\%}`, `\TaskCount{1}`, `\InvalidSemanticMax`} {
		want := "\\newcommand{" + strings.SplitN(m, "{", 2)[0] + "}"
		if !strings.Contains(string(macros), want) {
			t.Fatalf("macros missing %s:\n%s", want, macros)
		}
	}
	if !strings.Contains(string(macros), `\newcommand{\GlmSemanticPass}{100\%}`) {
		t.Fatalf("GlmSemanticPass wrong value:\n%s", macros)
	}
}

// Figures are generated the same way tables are: pgfplots bar charts whose
// coordinates come from the pooled per-profile, per-arm aggregate, so a
// figure cannot drift from the evidence it plots.
func TestPaperFiguresGenerated(t *testing.T) {
	dir := writeRunDir(t)
	out := t.TempDir()
	if err := paperTables([]string{dir}, out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pass_by_arm.tex", "invalid_by_arm.tex"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(b)
		if !strings.Contains(s, "\\begin{tikzpicture}") || !strings.Contains(s, "\\legend{raw, serena, semantic}") {
			t.Fatalf("%s not a grouped-arm chart:\n%s", f, s)
		}
		// The fixture's glm profile is a symbolic x coord in both charts.
		if !strings.Contains(s, "symbolic x coords={glm}") {
			t.Fatalf("%s missing glm coord:\n%s", f, s)
		}
	}
	// Fixture: glm semantic passes 1/1 (100), raw 0/1 (0).
	pass, _ := os.ReadFile(filepath.Join(out, "pass_by_arm.tex"))
	if !strings.Contains(string(pass), "(glm,100)") || !strings.Contains(string(pass), "(glm,0)") {
		t.Fatalf("pass chart missing expected coords:\n%s", pass)
	}
}
