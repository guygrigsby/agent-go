package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/guygrigsby/agent-go/bench"
)

type MoveSpec = bench.MoveSpec

// extractMoves recovers (pkg, sym, to_pkg) move specs from a ground-truth
// commit: a top-level declaration present in one package at the parent and
// in a different package at the commit, gone from the first.
func extractMoves(repo, sha string) ([]MoveSpec, string) {
	files := changedGoFiles(repo, sha)
	if len(files) == 0 {
		return nil, "no Go files in diff"
	}
	before := parseDecls(repo, sha+"^", files)
	after := parseDecls(repo, sha, files)
	var specs []MoveSpec
	for name, b := range before {
		a, ok := after[name]
		if !ok {
			continue
		}
		// Same top-level sym, different directory: only plain (non-member)
		// syms move; a Type.Field entry travels with its type.
		if strings.Contains(b.sym, ".") || b.sym != name || a.sym != name {
			continue
		}
		if path.Dir(b.file) == path.Dir(a.file) {
			continue
		}
		// The name must actually be gone from the source revision's file.
		if declStillIn(repo, sha, b.file, name) {
			continue
		}
		specs = append(specs, MoveSpec{
			Pkg:   pkgPath(repo, sha, b.file),
			Sym:   name,
			ToPkg: pkgPath(repo, sha, a.file),
		})
	}
	if len(specs) == 0 {
		return nil, "no declaration changed packages in the diff"
	}
	return specs, ""
}

// declStillIn reports whether rev's copy of file still declares name.
func declStillIn(repo, rev, file, name string) bool {
	decls := parseDecls(repo, rev, []string{file})
	_, ok := decls[name]
	return ok
}

// prepMove mirrors prepRename for move-kind tasks.
func prepMove(scratch, tasksFile, outFile string) error {
	var tasks []Task
	b, err := os.ReadFile(tasksFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &tasks); err != nil {
		return err
	}
	var out []Manifest
	clean := 0
	for _, t := range tasks {
		if !slices.Contains(t.Kinds, "move") {
			continue
		}
		repo := path.Join(scratch, t.Repo)
		m := Manifest{Repo: t.Repo, SHA: t.SHA, Kind: "move",
			Prompt: strings.TrimSpace(issueRef.ReplaceAllString(t.Subject, "")), GoFiles: t.GoFiles}
		m.Moves, m.NeedsReview = extractMoves(repo, t.SHA)
		if len(m.Moves) > 0 {
			clean++
			m.Prompt = ensureMovesNamed(m.Prompt, m.Moves)
		}
		out = append(out, m)
	}
	b, _ = json.MarshalIndent(out, "", " ")
	if err := os.WriteFile(outFile, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d/%d extracted cleanly -> %s\n", clean, len(out), outFile)
	return nil
}

func ensureMovesNamed(prompt string, specs []MoveSpec) string {
	var lines []string
	for _, s := range specs {
		if !strings.Contains(prompt, s.Sym) || !strings.Contains(prompt, path.Base(s.ToPkg)) {
			lines = append(lines, fmt.Sprintf("move %s from %s to %s", s.Sym, s.Pkg, s.ToPkg))
		}
	}
	if len(lines) == 0 {
		return prompt
	}
	return prompt + "\n\nSpecifically: " + strings.Join(lines, "; ")
}
