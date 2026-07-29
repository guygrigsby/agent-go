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
	"strconv"
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

// importRef is one entry of upsert_decl's "imports" field: a path the
// engine cannot infer on its own (an alias, or a name goimports would
// resolve ambiguously) plus the alias to use, empty for the default name.
type importRef struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// declEntry is one top-level, non-import declaration pulled from a
// ground-truth file, carrying enough to order it against its siblings and
// to attach its origin file's import block.
type declEntry struct {
	text     string
	provides map[string]bool // top-level names this decl declares
	requires map[string]bool // identifiers this decl's body/type references
	imports  []importRef     // origin file's import block
}

// authorOracleOps parses ground-truth implementation files into one atomic
// patch of upsert_decl ops, two rules load-bearing for oracle replay to be
// accepted:
//
//   - Order: the first op in a package with no non-test files takes the
//     file-creation path (createFileInPatch), which validates the decl
//     immediately with no ledger visibility, so it must not reference a
//     decl that lands later in the same patch. Ops after the first append
//     to the ledger and typecheck at end-of-patch, where the whole file is
//     assembled, so only the edge into the first op is truly load-bearing;
//     ops are nonetheless fully dependency-ordered (topological sort over
//     intra-package identifier references, computed across all files
//     together since the patch is one atomic assembly) for a stable,
//     predictable result. Ties and reference cycles keep file-sorted,
//     source order: the sort always advances on the earliest
//     not-yet-placed decl, whether because its dependencies are satisfied
//     or, on a cycle, because nothing's dependencies are.
//   - Imports: goimports resolves a bare package name against whatever it
//     finds first, which can be the wrong module when a name is ambiguous
//     (e.g. a bare "proto" resolving to the wrong package entirely). Each
//     op carries its origin file's own parsed import block (path plus
//     alias) via the "imports" field, so the engine uses the ground-truth
//     binding instead of guessing.
func authorOracleOps(pkg string, files map[string]string) ([]map[string]any, error) {
	var entries []declEntry
	fset := token.NewFileSet()
	for _, path := range slices.Sorted(maps.Keys(files)) {
		src := files[path]
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		imps := fileImports(f)
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
			entries = append(entries, declEntry{
				text:     text,
				provides: declProvides(d),
				requires: declRequires(d),
				imports:  imps,
			})
		}
	}

	ops := make([]map[string]any, len(entries))
	for i, idx := range depOrder(entries) {
		e := entries[idx]
		op := map[string]any{"op": "upsert_decl", "pkg": pkg, "text": e.text}
		if len(e.imports) > 0 {
			op["imports"] = e.imports
		}
		ops[i] = op
	}
	return ops, nil
}

// fileImports parses one file's import block into path/alias pairs. Blank
// (_) and dot (.) imports keep their alias like any other: only the
// default (no Name node) case omits it.
func fileImports(f *ast.File) []importRef {
	var out []importRef
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			path = spec.Path.Value
		}
		ref := importRef{Path: path}
		if spec.Name != nil {
			ref.Name = spec.Name.Name
		}
		out = append(out, ref)
	}
	return out
}

// declProvides returns the top-level names a decl introduces. Methods
// (FuncDecl with a receiver) provide nothing: they're never called by bare
// identifier, so they can't be another decl's forward-reference target.
func declProvides(d ast.Decl) map[string]bool {
	names := map[string]bool{}
	switch n := d.(type) {
	case *ast.FuncDecl:
		if n.Recv == nil {
			names[n.Name.Name] = true
		}
	case *ast.GenDecl:
		for _, spec := range n.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, id := range s.Names {
					names[id.Name] = true
				}
			case *ast.TypeSpec:
				names[s.Name.Name] = true
			}
		}
	}
	return names
}

// declRequires returns the identifiers a decl's signature/type and body
// reference, walked to exclude sites that name something rather than use
// it: a selector's field/method (pkg.Foo, recv.Method - only pkg/recv are
// real references), a composite literal's key (struct field or map key),
// and a field list's parameter/struct-field names (declarations, not
// uses; the field's type is still walked, so an embedded or field type
// naming another top-level decl still counts). It then subtracts every
// name the decl binds locally (localBoundNames): a local variable,
// parameter, or receiver that happens to share a spelling with an
// unrelated top-level decl is not a reference to it, and counting it as
// one manufactures a dependency edge that isn't there (the exact failure
// class this function exists to prevent, just one level down: a fake
// edge can build a fake cycle, and the cycle tie-break can then place a
// dependent decl first).
func declRequires(d ast.Decl) map[string]bool {
	refs := map[string]bool{}
	switch n := d.(type) {
	case *ast.FuncDecl:
		if n.Recv != nil {
			collectIdents(n.Recv, refs)
		}
		collectIdents(n.Type, refs)
		if n.Body != nil {
			collectIdents(n.Body, refs)
		}
	case *ast.GenDecl:
		for _, spec := range n.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				if s.Type != nil {
					collectIdents(s.Type, refs)
				}
				for _, v := range s.Values {
					collectIdents(v, refs)
				}
			case *ast.TypeSpec:
				collectIdents(s.Type, refs)
			}
		}
	}
	for name := range localBoundNames(d) {
		delete(refs, name)
	}
	return refs
}

// localBoundNames returns every identifier a decl binds locally rather
// than at package scope: the receiver and parameter/result names of the
// decl itself (and of any nested closure), short variable declarations
// (:=, including a range statement's and a type switch guard's define
// form), and local var/const/type declarations inside a body. None of
// these are references to a same-named top-level decl, so declRequires
// subtracts them.
//
// This is a flat collect-then-subtract, not a scope-tracking walk: a name
// is excluded from the whole decl the moment it's bound anywhere inside
// it, even outside that binding's actual lexical block. The only shape
// this misses is a local name that shadows a top-level decl in one block
// while the same spelling is legitimately used elsewhere in the same decl
// to mean the top-level decl; that's rare, and the cost of missing it is
// only a weaker ordering (falls back to source order), never a dropped or
// duplicated op.
func localBoundNames(d ast.Decl) map[string]bool {
	names := map[string]bool{}
	bindIdent := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			names[id.Name] = true
		}
	}
	bindFields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, f := range list.List {
			for _, id := range f.Names {
				if id.Name != "_" {
					names[id.Name] = true
				}
			}
		}
	}
	if fn, ok := d.(*ast.FuncDecl); ok {
		bindFields(fn.Recv)
	}
	ast.Inspect(d, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncType:
			bindFields(x.Params)
			bindFields(x.Results)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, l := range x.Lhs {
					bindIdent(l)
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				bindIdent(x.Key)
				bindIdent(x.Value)
			}
		case *ast.DeclStmt:
			if g, ok := x.Decl.(*ast.GenDecl); ok {
				for _, spec := range g.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if id.Name != "_" {
								names[id.Name] = true
							}
						}
					case *ast.TypeSpec:
						names[s.Name.Name] = true
					}
				}
			}
		case *ast.LabeledStmt:
			names[x.Label.Name] = true
		}
		return true
	})
	return names
}

// collectIdents walks n collecting identifier names that are genuine
// uses, per the exclusions documented on declRequires.
func collectIdents(n ast.Node, into map[string]bool) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.SelectorExpr:
			collectIdents(x.X, into)
			return false
		case *ast.KeyValueExpr:
			collectIdents(x.Value, into)
			return false
		case *ast.Field:
			collectIdents(x.Type, into)
			return false
		case *ast.Ident:
			into[x.Name] = true
		}
		return true
	})
}

// depOrder returns a permutation of entries' indices: a topological sort
// by intra-package reference (a decl referencing another lands after it),
// breaking every tie, including reference cycles, toward the earliest
// not-yet-placed original index. That single rule covers both cases: with
// no cycle it's Kahn's algorithm scanning candidates left to right, and
// on a cycle (no decl currently has all its dependencies placed) it
// forces the earliest remaining decl through anyway, guaranteeing
// progress every iteration, so the sort always terminates in exactly
// len(entries) steps and never drops a decl.
func depOrder(entries []declEntry) []int {
	owner := map[string]int{}
	for i, e := range entries {
		for name := range e.provides {
			owner[name] = i
		}
	}
	// dependsOn[i] is the set of entry indices i must land after.
	dependsOn := make([]map[int]bool, len(entries))
	for i, e := range entries {
		deps := map[int]bool{}
		for name := range e.requires {
			if j, ok := owner[name]; ok && j != i {
				deps[j] = true
			}
		}
		dependsOn[i] = deps
	}
	indegree := make([]int, len(entries))
	dependents := make([][]int, len(entries))
	for i, deps := range dependsOn {
		indegree[i] = len(deps)
		for j := range deps {
			dependents[j] = append(dependents[j], i)
		}
	}

	done := make([]bool, len(entries))
	order := make([]int, 0, len(entries))
	for range entries {
		next := -1
		for i := range entries {
			if !done[i] && indegree[i] == 0 {
				next = i
				break
			}
		}
		if next == -1 {
			// Reference cycle: nothing is ready. Force the earliest
			// remaining decl so the sort still advances.
			for i := range entries {
				if !done[i] {
					next = i
					break
				}
			}
		}
		done[next] = true
		order = append(order, next)
		for _, i := range dependents[next] {
			indegree[i]--
		}
	}
	return order
}
