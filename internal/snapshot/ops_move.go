package snapshot

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// moveDeclEdits computes a whole-declaration move: delete from the source
// file, append to a target-package file, and requalify every reference.
// v1 ceiling: the declaration must be self-contained (no references to
// other package-level symbols of its own package — moving those would need
// a dependency-closure move) and a moved type may not have methods yet.
func moveDeclEdits(s *Snapshot, pkgPath, sym, toPkg string) ([]edit, *Reject) {
	if strings.Contains(sym, ".") {
		return nil, &Reject{Reason: "move_decl moves top-level declarations",
			Detail: sym + " names a member; move the containing declaration"}
	}
	if toPkg == pkgPath {
		return nil, &Reject{Reason: "declaration is already in that package", Detail: toPkg}
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	tgt := s.primary(toPkg)
	if tgt == nil || tgt.Types == nil {
		return nil, &Reject{Reason: "target package not found", Detail: toPkg,
			DidYouMean: s.suggestPackages(toPkg)}
	}
	if tn, ok := obj.(*types.TypeName); ok {
		// ponytail: methods would have to move too; reject until the
		// dependency-closure move exists.
		if types.NewMethodSet(types.NewPointer(tn.Type())).Len() > 0 {
			return nil, &Reject{Reason: "type has methods; move_decl v1 moves method-free declarations",
				Detail: sym}
		}
	}
	declFile, start, end := s.findDeclRange(p, obj.Name(), sym)
	if declFile == "" {
		return nil, &Reject{Reason: "declaration not found", Detail: sym}
	}
	declSrc, err := os.ReadFile(declFile)
	if err != nil {
		return nil, &Reject{Reason: "file not found", Detail: declFile}
	}
	declText := string(declSrc[start:end])

	// Self-containedness: any use of another package-level symbol from the
	// same package can't survive the move without dragging it along.
	var deps []string
	depSeen := map[string]bool{}
	for _, f := range p.Syntax {
		if f.Pos() == 0 || s.fset.Position(f.Pos()).Filename != declFile {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			off := s.fset.Position(id.Pos()).Offset
			if off < start || off >= end {
				return true
			}
			o := p.TypesInfo.Uses[id]
			if o == nil || o == obj || o.Pkg() != p.Types {
				return true
			}
			if o.Parent() == p.Types.Scope() && !depSeen[o.Name()] {
				depSeen[o.Name()] = true
				deps = append(deps, o.Name())
			}
			return true
		})
	}
	if len(deps) > 0 {
		sort.Strings(deps)
		return nil, &Reject{Reason: "declaration depends on package-local symbols; move_decl v1 moves self-contained declarations",
			Detail: sym + " uses " + strings.Join(deps, ", ")}
	}

	edits := []edit{{declFile, start, end - start, ""}}

	// Land in the target's first non-test file, appended at the end; the
	// patch pipeline's imports.Process fixes both files' import blocks.
	tgtFile := ""
	for _, f := range tgt.CompiledGoFiles {
		if !strings.HasSuffix(f, "_test.go") {
			tgtFile = f
			break
		}
	}
	if tgtFile == "" {
		return nil, &Reject{Reason: "target package has no non-test file", Detail: toPkg}
	}
	tgtSrc, err := os.ReadFile(tgtFile)
	if err != nil {
		return nil, &Reject{Reason: "file not found", Detail: tgtFile}
	}
	edits = append(edits, edit{tgtFile, len(tgtSrc), 0, "\n\n" + declText + "\n"})

	// Requalify references. Qualified refs swap their package qualifier;
	// bare same-package refs gain the target's package name. goimports
	// cannot reliably resolve module-local packages offline, so each file
	// gaining a qualifier also gains the import explicitly.
	tgtName := tgt.Types.Name()
	key := s.objKey(obj)
	seen := map[string]bool{}
	importAdded := map[string]bool{}
	srcBytes := map[string][]byte{declFile: declSrc}
	for _, p2 := range s.pkgs {
		if p2.TypesInfo == nil || strings.HasSuffix(p2.ID, ".test") {
			continue
		}
		// Selector parents, so a qualified ref rewrites qualifier and all.
		selOf := map[*ast.Ident]*ast.SelectorExpr{}
		for _, f := range p2.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					selOf[sel.Sel] = sel
				}
				return true
			})
		}
		for id, o := range p2.TypesInfo.Uses {
			if o == nil || o.Name() != obj.Name() || s.objKey(o) != key {
				continue
			}
			pos := p2.Fset.Position(id.Pos())
			if seen[pos.String()] {
				continue
			}
			seen[pos.String()] = true
			off := pos.Offset
			if pos.Filename == declFile && off >= start && off < end {
				continue // a self-reference moves with the text
			}
			from, length := off, len(obj.Name())
			text := tgtName + "." + obj.Name()
			if sel, ok := selOf[id]; ok {
				if x, isIdent := sel.X.(*ast.Ident); isIdent {
					if _, isPkg := p2.TypesInfo.Uses[x].(*types.PkgName); isPkg {
						from = p2.Fset.Position(x.Pos()).Offset
						length = p2.Fset.Position(sel.End()).Offset - from
					}
				}
			}
			if p2.PkgPath == toPkg {
				text = obj.Name()
			}
			if srcBytes[pos.Filename] == nil {
				b, err := os.ReadFile(pos.Filename)
				if err != nil {
					return nil, &Reject{Reason: "file not found", Detail: pos.Filename}
				}
				srcBytes[pos.Filename] = b
			}
			edits = append(edits, edit{pos.Filename, from, length, text})
			if p2.PkgPath != toPkg && !importAdded[pos.Filename] {
				importAdded[pos.Filename] = true
				if e := importEdit(s, p2, pos.Filename, toPkg); e != nil {
					edits = append(edits, *e)
				}
			}
		}
	}
	return edits, nil
}

// importEdit inserts an import of path into file, or nil when it already
// imports it. gofmt/goimports later canonicalizes block ordering; this
// only guarantees presence, which the offline resolver cannot.
func importEdit(s *Snapshot, p *packages.Package, file, path string) *edit {
	for _, f := range p.Syntax {
		if s.fset.Position(f.Pos()).Filename != file {
			continue
		}
		var lastImp *ast.GenDecl
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.IMPORT {
				continue
			}
			lastImp = gd
			for _, spec := range gd.Specs {
				if is, ok := spec.(*ast.ImportSpec); ok &&
					strings.Trim(is.Path.Value, `"`) == path {
					return nil
				}
			}
		}
		if lastImp != nil && lastImp.Rparen.IsValid() {
			off := s.fset.Position(lastImp.Rparen).Offset
			return &edit{file, off, 0, "\t\"" + path + "\"\n"}
		}
		if lastImp != nil {
			off := s.fset.Position(lastImp.End()).Offset
			return &edit{file, off, 0, "\nimport \"" + path + "\""}
		}
		off := s.fset.Position(f.Name.End()).Offset
		return &edit{file, off, 0, "\n\nimport \"" + path + "\""}
	}
	return nil
}

type moveDeclOp struct{}

func (moveDeclOp) name() string { return "move_decl" }

func (moveDeclOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg       string `json:"pkg"`
		Sym       string `json:"sym"`
		ToPkg     string `json:"to_pkg"`
		CreatePkg bool   `json:"create_pkg"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	if a.ToPkg == "" {
		return &Reject{Reason: "to_pkg is required"}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	if a.CreatePkg && ctx.s.primary(a.ToPkg) == nil {
		// Opt-in only: a typo'd to_pkg must reject with candidates, never
		// silently create a package. The created file is just the package
		// clause; moveDeclEdits below appends the declaration to it.
		s := ctx.s
		if len(s.pkgs) == 0 || s.pkgs[0].Module == nil {
			return &Reject{Reason: "target package not found", Detail: a.ToPkg}
		}
		mod := s.pkgs[0].Module
		rel, ok := strings.CutPrefix(a.ToPkg, mod.Path+"/")
		if !ok {
			return &Reject{Reason: "package is outside the module",
				Detail: a.ToPkg + " not under " + mod.Path}
		}
		file := filepath.Join(mod.Dir, rel, "agent.go")
		if rej := createFileInPatch(ctx, a.ToPkg, file, "package "+filepath.Base(rel)+"\n", ""); rej != nil {
			return rej
		}
	}
	edits, rej := moveDeclEdits(ctx.s, pkg, sym, a.ToPkg)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits(edits); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.addAffected(a.ToPkg)
	ctx.noteTouched(a.ToPkg, sym, false)
	return nil
}

func init() {
	opRegistry["move_decl"] = func() patchOp { return moveDeclOp{} }
	declOps["move_decl"] = true
}
