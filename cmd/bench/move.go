package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/guygrigsby/agent-go/bench"
)

type MoveSpec = bench.MoveSpec

// wholePackageCap bounds a task's move specs: beyond it the commit is a
// whole-package or directory relocation, out of task scope.
const wholePackageCap = 8

// extractMoves recovers move specs from a ground-truth commit. Two
// pairings, both per directory rather than first-file-wins (a same-named
// declaration in an unrelated changed package must not eat the move):
//
//  1. Same name, different directory: the name left exactly one dir and
//     arrived in exactly one other.
//  2. Move+rename compounds (vault 45b0179a: mergeStates became
//     MergeReplicationStates on its way to api): names that vanished
//     entirely pair with names that appeared entirely by body
//     fingerprint, with every vanished/appeared name normalized out of
//     the text first so renamed siblings that call each other still
//     match. A unique fingerprint pair with a directory change is a
//     compound spec carrying to_name.
func extractMoves(repo, sha string) ([]MoveSpec, string) {
	files := changedGoFiles(repo, sha)
	if len(files) == 0 {
		return nil, "no Go files in diff"
	}
	before := parseDeclEntries(repo, sha+"^", files)
	after := parseDeclEntries(repo, sha, files)
	bBy, aBy := groupByName(before), groupByName(after)

	var specs []MoveSpec
	for _, name := range sortedKeys(bBy) {
		bs, as := bBy[name], aBy[name]
		if len(as) == 0 {
			continue // vanished entirely; the fingerprint pass owns it
		}
		bd, ad := dirSet(bs), dirSet(as)
		var lost, gained []declEntry
		for _, e := range bs {
			if !ad[path.Dir(e.file)] {
				lost = append(lost, e)
			}
		}
		for _, e := range as {
			if !bd[path.Dir(e.file)] {
				gained = append(gained, e)
			}
		}
		if len(lost) == 1 && len(gained) == 1 {
			specs = append(specs, MoveSpec{
				Pkg:   pkgPath(repo, sha, lost[0].file),
				Sym:   name,
				ToPkg: pkgPath(repo, sha, gained[0].file),
			})
		}
	}

	var vanished, appeared []declEntry
	var vNames, aNames []string
	for _, name := range sortedKeys(bBy) {
		if len(aBy[name]) == 0 {
			vanished = append(vanished, bBy[name]...)
			vNames = append(vNames, name)
		}
	}
	for _, name := range sortedKeys(aBy) {
		if len(bBy[name]) == 0 {
			appeared = append(appeared, aBy[name]...)
			aNames = append(aNames, name)
		}
	}
	if len(vanished) > 0 && len(appeared) > 0 {
		bFP := groupByFingerprint(vanished, vNames)
		aFP := groupByFingerprint(appeared, aNames)
		for _, fp := range sortedKeys(bFP) {
			b, a := bFP[fp], aFP[fp]
			if len(b) != 1 || len(a) != 1 || path.Dir(b[0].file) == path.Dir(a[0].file) {
				continue
			}
			specs = append(specs, MoveSpec{
				Pkg:    pkgPath(repo, sha, b[0].file),
				Sym:    b[0].name,
				ToPkg:  pkgPath(repo, sha, a[0].file),
				ToName: a[0].name,
			})
		}
	}

	if len(specs) == 0 {
		return nil, "no declaration changed packages in the diff"
	}
	if len(specs) > wholePackageCap {
		return nil, fmt.Sprintf("whole package or directory move (%d declarations relocated); out of task scope", len(specs))
	}
	return specs, ""
}

// declEntry is one plain top-level declaration with its source text, for
// directory-aware and fingerprint pairing.
type declEntry struct {
	file, name, text string
}

func groupByName(entries []declEntry) map[string][]declEntry {
	by := map[string][]declEntry{}
	for _, e := range entries {
		by[e.name] = append(by[e.name], e)
	}
	return by
}

func dirSet(entries []declEntry) map[string]bool {
	set := map[string]bool{}
	for _, e := range entries {
		set[path.Dir(e.file)] = true
	}
	return set
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// groupByFingerprint normalizes each entry's text (every name in names
// replaced by a placeholder, whitespace collapsed) and groups by the
// result: two declarations that differ only by the renamed identifiers
// share a fingerprint.
func groupByFingerprint(entries []declEntry, names []string) map[string][]declEntry {
	res := make([]*regexp.Regexp, len(names))
	for i, n := range names {
		res[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`)
	}
	ws := regexp.MustCompile(`\s+`)
	by := map[string][]declEntry{}
	for _, e := range entries {
		fp := e.text
		for _, re := range res {
			fp = re.ReplaceAllString(fp, "\x01")
		}
		fp = ws.ReplaceAllString(fp, "")
		by[fp] = append(by[fp], e)
	}
	return by
}

// parseDeclEntries collects plain top-level declarations (funcs without
// receivers, types, single-name values) with their source text. Methods
// and fields travel with their types and are not independently movable.
func parseDeclEntries(repo, rev string, files []string) []declEntry {
	var entries []declEntry
	for _, f := range files {
		src := gitShow(repo, rev+":"+f)
		if src == "" {
			continue
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		slice := func(n ast.Node) string {
			s, e := fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset
			if s < 0 || e > len(src) || s >= e {
				return ""
			}
			return src[s:e]
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name != "_" {
					entries = append(entries, declEntry{f, d.Name.Name, slice(d)})
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						entries = append(entries, declEntry{f, spec.Name.Name, slice(spec)})
					case *ast.ValueSpec:
						if len(spec.Names) == 1 && spec.Names[0].Name != "_" {
							entries = append(entries, declEntry{f, spec.Names[0].Name, slice(spec)})
						}
					}
				}
			}
		}
	}
	return entries
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
			line := fmt.Sprintf("move %s from %s to %s", s.Sym, s.Pkg, s.ToPkg)
			if s.ToName != "" {
				line += fmt.Sprintf(" renaming it to %s", s.ToName)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return prompt
	}
	return prompt + "\n\nSpecifically: " + strings.Join(lines, "; ")
}
