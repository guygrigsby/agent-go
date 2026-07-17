package snapshot

import (
	"encoding/json"
	"fmt"
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

	declFile, start, end := s.findDeclRange(p, obj.Name(), sym)
	grouped := ""
	if declFile == "" {
		// Not a standalone declaration: it may be one spec inside a grouped
		// const/var/type block (boundary b26814a3's RecoveryUserId). The
		// spec is extracted standalone into the target and deleted from the
		// group; the group and its siblings stay put.
		var gd *ast.GenDecl
		var sp ast.Spec
		declFile, gd, sp = s.findGroupedSpec(p, obj.Name())
		if declFile == "" {
			return nil, &Reject{Reason: "declaration not found", Detail: sym}
		}
		if rej := groupedSpecMovable(sp); rej != nil {
			return nil, rej
		}
		start, end = s.specRange(sp)
		grouped = gd.Tok.String()
	}
	// The move is a span set: the declaration itself, plus the whole
	// method set when it is a type with methods (the dependency-closure
	// move; boundary 687dd1bd relocates a job type and its methods as one
	// unit). Closure-internal references are legal by construction: method
	// objects live outside the package scope, so the dependency scan below
	// never flags a method calling its sibling or naming its receiver.
	type span struct {
		file       string
		start, end int
	}
	spans := []span{{declFile, start, end}}
	if tn, ok := obj.(*types.TypeName); ok {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, isFn := d.(*ast.FuncDecl)
				if !isFn || fd.Recv == nil || recvTypeName(fd) != tn.Name() {
					continue
				}
				st := fd.Pos()
				if fd.Doc != nil {
					st = fd.Doc.Pos()
				}
				spans = append(spans, span{s.fset.Position(fd.Pos()).Filename,
					s.fset.Position(st).Offset, s.fset.Position(fd.End()).Offset})
			}
		}
	}
	srcByFile := map[string][]byte{}
	for _, sp := range spans {
		if srcByFile[sp.file] == nil {
			b, err := os.ReadFile(sp.file)
			if err != nil {
				return nil, &Reject{Reason: "file not found", Detail: sp.file}
			}
			srcByFile[sp.file] = b
		}
	}
	declSrc := srcByFile[declFile]
	inSpans := func(file string, off int) bool {
		for _, sp := range spans {
			if sp.file == file && off >= sp.start && off < sp.end {
				return true
			}
		}
		return false
	}

	// Self-containedness: any use of another package-level symbol from the
	// same package can't survive the move without dragging it along. The
	// same scan collects the imports the declaration actually uses (alias
	// included) so they travel with it: goimports cannot reconstruct an
	// alias or pick between same-named packages, so the move never trusts
	// it for the carried decl's own imports.
	var deps []string
	depSeen := map[string]bool{}
	carried := map[string]string{} // import path -> alias ("" when default)
	qualCuts := map[int][]int{}    // span index -> span-relative qualifier offsets to strip
	spanIdx := func(file string, off int) int {
		for i, sp := range spans {
			if sp.file == file && off >= sp.start && off < sp.end {
				return i
			}
		}
		return -1
	}
	for _, f := range p.Syntax {
		fname := s.fset.Position(f.Pos()).Filename
		if f.Pos() == 0 || srcByFile[fname] == nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			off := s.fset.Position(id.Pos()).Offset
			if !inSpans(fname, off) {
				return true
			}
			o := p.TypesInfo.Uses[id]
			if o == nil || o == obj {
				return true
			}
			if pn, isPkg := o.(*types.PkgName); isPkg {
				path := pn.Imported().Path()
				if path != toPkg {
					alias := ""
					if id.Name != pn.Imported().Name() {
						alias = id.Name
					}
					carried[path] = alias
				} else {
					// The decl is moving INTO this package: the qualifier
					// (and its dot) must go, or goimports resolves the
					// dangling name to whatever same-named package the
					// module cache offers.
					if i := spanIdx(fname, off); i >= 0 {
						qualCuts[i] = append(qualCuts[i], off-spans[i].start)
					}
				}
				return true
			}
			if o.Pkg() != p.Types {
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

	var declTexts []string
	var edits []edit
	for i, sp := range spans {
		text := string(srcByFile[sp.file][sp.start:sp.end])
		if cuts := qualCuts[i]; len(cuts) > 0 {
			sort.Sort(sort.Reverse(sort.IntSlice(cuts)))
			for _, c := range cuts {
				// The qualifier ident plus its trailing dot; gofmt
				// guarantees no space between them.
				qualLen := strings.Index(text[c:], ".") + 1
				text = text[:c] + text[c+qualLen:]
			}
		}
		delEnd := sp.end
		if i == 0 && grouped != "" {
			text = grouped + " " + text
			// Consume the spec's own line ending so the group doesn't
			// keep a blank line where the spec was.
			if delEnd < len(srcByFile[sp.file]) && srcByFile[sp.file][delEnd] == '\n' {
				delEnd++
			}
		}
		declTexts = append(declTexts, text)
		edits = append(edits, edit{sp.file, sp.start, delEnd - sp.start, ""})
	}
	declText := strings.Join(declTexts, "\n\n")

	// Land in a target file of the same test-ness as the source: a decl
	// from a _test.go must land in a _test.go (go test ignores it anywhere
	// else), everything else in a non-test file. Test files live on the
	// test-variant package, so collect candidates across every variant of
	// toPkg. Appended at the end; the patch pipeline's imports.Process
	// fixes both files' import blocks.
	fromTest := strings.HasSuffix(declFile, "_test.go")
	tgtFile := ""
	tgtOwner := tgt
	for _, tp := range s.pkgs {
		if tp.PkgPath != toPkg {
			continue
		}
		for _, f := range tp.CompiledGoFiles {
			if strings.HasSuffix(f, "_test.go") == fromTest {
				tgtFile = f
				tgtOwner = tp
				break
			}
		}
		if tgtFile != "" {
			break
		}
	}
	if tgtFile == "" {
		if fromTest {
			return nil, &Reject{Reason: "target package has no test file", Detail: toPkg}
		}
		return nil, &Reject{Reason: "target package has no non-test file", Detail: toPkg}
	}
	tgtSrc, err := os.ReadFile(tgtFile)
	if err != nil {
		return nil, &Reject{Reason: "file not found", Detail: tgtFile}
	}
	edits = append(edits, edit{tgtFile, len(tgtSrc), 0, "\n\n" + declText + "\n"})
	carriedPaths := make([]string, 0, len(carried))
	for path := range carried {
		carriedPaths = append(carriedPaths, path)
	}
	sort.Strings(carriedPaths)
	for _, path := range carriedPaths {
		if e := importEdit(s, tgtOwner, tgtFile, path, carried[path]); e != nil {
			edits = append(edits, *e)
		}
	}

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
			if inSpans(pos.Filename, off) {
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
				if e := importEdit(s, p2, pos.Filename, toPkg, ""); e != nil {
					edits = append(edits, *e)
				}
			}
		}
	}
	return edits, nil
}

// findGroupedSpec locates name as one spec inside a grouped const/var/type
// block (two or more specs; single-spec GenDecls are findDeclNode's turf).
func (s *Snapshot) findGroupedSpec(p *packages.Package, name string) (string, *ast.GenDecl, ast.Spec) {
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || len(gd.Specs) < 2 {
				continue
			}
			for _, sp := range gd.Specs {
				switch sp := sp.(type) {
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.Name == name {
							return s.fset.Position(f.Pos()).Filename, gd, sp
						}
					}
				case *ast.TypeSpec:
					if sp.Name.Name == name {
						return s.fset.Position(f.Pos()).Filename, gd, sp
					}
				}
			}
		}
	}
	return "", nil, nil
}

// groupedSpecMovable rejects specs that cannot stand alone outside their
// group: multi-name lines, values inherited from the previous spec, and
// anything leaning on iota (position-dependent by definition).
func groupedSpecMovable(sp ast.Spec) *Reject {
	vs, ok := sp.(*ast.ValueSpec)
	if !ok {
		return nil // TypeSpecs are always self-complete
	}
	if len(vs.Names) != 1 {
		return &Reject{Reason: "spec declares several names; move_decl v1 moves single-name specs",
			Detail: fmt.Sprint(vs.Names)}
	}
	if len(vs.Values) == 0 && vs.Type == nil {
		return &Reject{Reason: "spec inherits its value from the previous line; it cannot stand alone outside its group",
			Detail: vs.Names[0].Name}
	}
	usesIota := false
	for _, v := range vs.Values {
		ast.Inspect(v, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
				usesIota = true
			}
			return true
		})
	}
	if usesIota {
		return &Reject{Reason: "spec uses iota; its value is defined by its position in the group",
			Detail: vs.Names[0].Name}
	}
	return nil
}

// specRange is a spec's byte range including its doc comment and trailing
// line comment.
func (s *Snapshot) specRange(sp ast.Spec) (int, int) {
	start, end := sp.Pos(), sp.End()
	switch sp := sp.(type) {
	case *ast.ValueSpec:
		if sp.Doc != nil {
			start = sp.Doc.Pos()
		}
		if sp.Comment != nil {
			end = sp.Comment.End()
		}
	case *ast.TypeSpec:
		if sp.Doc != nil {
			start = sp.Doc.Pos()
		}
		if sp.Comment != nil {
			end = sp.Comment.End()
		}
	}
	return s.fset.Position(start).Offset, s.fset.Position(end).Offset
}

// importEdit inserts an import of path into file, or nil when it already
// imports it. gofmt/goimports later canonicalizes block ordering; this
// only guarantees presence, which the offline resolver cannot.
func importEdit(s *Snapshot, p *packages.Package, file, path, name string) *edit {
	spec := "\"" + path + "\""
	if name != "" {
		spec = name + " " + spec
	}
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
			return &edit{file, off, 0, "\t" + spec + "\n"}
		}
		if lastImp != nil {
			off := s.fset.Position(lastImp.End()).Offset
			return &edit{file, off, 0, "\nimport " + spec}
		}
		off := s.fset.Position(f.Name.End()).Offset
		return &edit{file, off, 0, "\n\nimport " + spec}
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
		// A directory that already holds files but no loaded package is a
		// nested module or an excluded dir (vault's sdk/), never a create
		// target — writing there would truncate real code.
		if ents, err := os.ReadDir(filepath.Dir(file)); err == nil && len(ents) > 0 {
			return &Reject{Reason: "package exists but did not load", Detail: a.ToPkg +
				": its directory already holds files (a nested module or an excluded dir); move_decl cannot target packages outside the loaded workspace"}
		}
		if rej := createFileInPatch(ctx, a.ToPkg, file, "package "+filepath.Base(rel)+"\n", ""); rej != nil {
			return rej
		}
	}
	edits, rej := moveDeclEdits(ctx.s, pkg, sym, a.ToPkg)
	if rej != nil && rej.Reason == "target package has no test file" {
		// Moving a test declaration needs a _test.go to land in; creating
		// one is unambiguous (no typo risk — the target package itself
		// already resolved), so no opt-in flag. Internal test package only:
		// the created file carries the target's own package clause.
		tgt := ctx.s.primary(a.ToPkg)
		if tgt == nil || len(tgt.CompiledGoFiles) == 0 {
			return rej
		}
		file := filepath.Join(filepath.Dir(tgt.CompiledGoFiles[0]), "agent_test.go")
		if crej := createFileInPatch(ctx, a.ToPkg, file, "package "+tgt.Types.Name()+"\n", ""); crej != nil {
			return crej
		}
		edits, rej = moveDeclEdits(ctx.s, pkg, sym, a.ToPkg)
	}
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
