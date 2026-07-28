package bench

// Author kind helpers: the ground-truth commit's tests are the spec
// (docs/specs/authoring-bench.md). The harness lands them pre-episode and
// scoring demands them back byte for byte; deleting or editing the test
// is the shortest path to green, so the freeze fails the episode
// outright.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// writeAuthorSpec lands the frozen spec tests in a fresh worktree.
func writeAuthorSpec(wt string, spec *AuthorSpec) error {
	for _, f := range spec.TestFiles {
		abs := filepath.Join(wt, f.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// specFilesIntact reports whether every spec test file still carries its
// frozen bytes; reason names the first divergence.
func specFilesIntact(wt string, spec *AuthorSpec) (string, bool) {
	for _, f := range spec.TestFiles {
		b, err := os.ReadFile(filepath.Join(wt, f.Path))
		if err != nil {
			return fmt.Sprintf("spec test %s is gone: %v", f.Path, err), false
		}
		if string(b) != f.Content {
			return fmt.Sprintf("spec test %s was modified", f.Path), false
		}
	}
	return "", true
}

// authorOracleOps parses ground-truth implementation files into one
// atomic patch of upsert_decl ops, sorted file order then source order.
// One patch, not sequential upserts: the package typechecks whole, so
// intra-package declaration order never rejects. Import blocks are
// skipped; the engine manages imports (set_body/upsert goimports sugar).
func authorOracleOps(pkg string, files map[string]string) ([]map[string]any, error) {
	var ops []map[string]any
	fset := token.NewFileSet()
	for _, path := range slices.Sorted(maps.Keys(files)) {
		src := files[path]
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				continue
			}
			start := d.Pos()
			switch n := d.(type) {
			case *ast.FuncDecl:
				if n.Doc != nil {
					start = n.Doc.Pos()
				}
			case *ast.GenDecl:
				if n.Doc != nil {
					start = n.Doc.Pos()
				}
			}
			text := src[fset.Position(start).Offset:fset.Position(d.End()).Offset]
			ops = append(ops, map[string]any{"op": "upsert_decl", "pkg": pkg, "text": text})
		}
	}
	return ops, nil
}
