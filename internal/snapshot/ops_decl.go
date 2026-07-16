package snapshot

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
)

// setBodyOp is set_body's composable form: same setBodyEdit core (locate
// the function body's brace offsets), applied to ctx.src through the
// decl-op ledger instead of straight to disk. Formatting is deferred to
// patchComposable's end-of-list imports.Process rather than the direct
// SetBody method's own format.Source call.
type setBodyOp struct{}

func (setBodyOp) name() string { return "set_body" }

func (setBodyOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Sym  string `json:"sym"`
		Body string `json:"body"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	file, lbrace, rbrace, baseline, rej := setBodyEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(baseline)
	if rej := ctx.applyDeclEdits([]edit{{file, lbrace, rbrace - lbrace + 1, "{\n" + a.Body + "\n}"}}); rej != nil {
		return rej
	}
	ctx.noteTouched(pkg, sym, false)
	return nil
}

// upsertDeclOp is upsert_decl's composable form: same upsertDeclEdit core
// (locate an existing declaration to replace, or an existing agent.go to
// append to), applied through the decl-op ledger. A brand-new agent.go or a
// brand-new package is created via createFileInPatch, so a patch can create
// a package and move declarations into it atomically.
type upsertDeclOp struct{}

func (upsertDeclOp) name() string { return "upsert_decl" }

func (upsertDeclOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Text string `json:"text"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	name, sym, rej := parseDeclText(a.Text)
	if rej != nil {
		return rej
	}
	if ctx.s.primary(pkg) == nil {
		// Brand-new package: mirror upsertNewPackage (upsert.go) inside the
		// patch via the same write-and-reload path a brand-new file takes.
		s := ctx.s
		if len(s.pkgs) == 0 || s.pkgs[0].Module == nil {
			return &Reject{Reason: "package not found", Detail: pkg,
				DidYouMean: s.suggestPackages(pkg)}
		}
		mod := s.pkgs[0].Module
		rel, ok := strings.CutPrefix(pkg, mod.Path+"/")
		if !ok {
			return &Reject{Reason: "package is outside the module",
				Detail: pkg + " not under " + mod.Path}
		}
		file := filepath.Join(mod.Dir, rel, "agent.go")
		if _, err := os.Stat(file); err == nil {
			return &Reject{Reason: "package exists but did not load", Detail: pkg}
		}
		src := "package " + filepath.Base(rel) + "\n\n" + strings.TrimSpace(a.Text) + "\n"
		return createFileInPatch(ctx, pkg, file, src, sym)
	}
	file, start, end, _, needsCreate, rej := upsertDeclEdit(ctx.s, pkg, name, sym)
	if rej != nil {
		return rej
	}
	if needsCreate {
		p := ctx.s.primary(pkg)
		src := "package " + p.Types.Name() + "\n\n" + strings.TrimSpace(a.Text) + "\n"
		return createFileInPatch(ctx, pkg, file, src, sym)
	}
	ctx.addBaseline(preflightBaseline(ctx.s, file, pkg))
	prefix := ""
	if start > 0 {
		prefix = "\n"
	}
	text := prefix + strings.TrimSpace(a.Text) + "\n"
	if rej := ctx.applyDeclEdits([]edit{{file, start, end - start, text}}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.noteTouched(pkg, sym, false)
	ctx.postChecks = append(ctx.postChecks, func() *Reject {
		if _, _, rej := ctx.s.findObject(pkg, sym); rej != nil {
			return &Reject{Reason: "declaration missing after edit", Detail: sym}
		}
		return nil
	})
	return nil
}

// createFileInPatch is the one way a changed file set becomes visible
// mid-patch (mirrors add_test's new-file path, ops_testgen.go): parse-check
// the generated source, write the brand-new file, force a full workspace
// reload, and reject on NEW workspace diagnostics. The baseline and the
// check are both workspace-wide because the reload's blast radius is.
// cleanupFileOps owns removal on every failure and dry_run path once
// the file registers in ctx.createdFiles; later ops in the same patch
// compose against the reloaded snapshot, where the file simply exists.
func createFileInPatch(ctx *patchCtx, pkg, file, src, sym string) *Reject {
	fixed, err := imports.Process(file, []byte(src), nil)
	if err != nil {
		return &Reject{Reason: "declaration does not parse in place", Detail: err.Error()}
	}
	before := errorSet(ctx.s.errors())
	ctx.addBaseline(before)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return &Reject{Reason: "failed to create file", Detail: err.Error()}
	}
	if err := os.WriteFile(file, fixed, 0o644); err != nil {
		return &Reject{Reason: "failed to create file", Detail: err.Error()}
	}
	ctx.createdFiles = append(ctx.createdFiles, file)
	ctx.s.loaded = false
	if _, err := ctx.s.load(); err != nil {
		return &Reject{Reason: "workspace failed to reload", Detail: err.Error()}
	}
	if diags := filterNew(ctx.s.errors(), before); len(diags) > 0 {
		return diagnosticRepairs(&Reject{Reason: "declaration does not typecheck", Diagnostics: diags})
	}
	// An empty sym means the file carries only its package clause
	// (move_decl's create_pkg path); there is no declaration to verify or
	// note.
	if sym == "" {
		ctx.s.noteWrite(file)
		ctx.addAffected(pkg)
		return nil
	}
	if _, _, rej := ctx.s.findObject(pkg, sym); rej != nil {
		return &Reject{Reason: "declaration missing after edit", Detail: pkg + "." + sym}
	}
	ctx.s.noteWrite(file)
	ctx.addAffected(pkg)
	ctx.noteTouched(pkg, sym, false)
	return nil
}

// preflightBaseline captures the pre-existing diagnostics every decl op's
// core observes before computing its edit: the dirty set is file's own
// compiling packages plus pkgPath's transitive reverse importers. Mutations
// no longer refuse on this rot — real-world repos carry unrelated breakage —
// instead each op folds the baseline into patchCtx (ctx.addBaseline) and
// patchComposable filters its end-of-list retypecheck against it, rejecting
// only when NEW diagnostics appear.
func preflightBaseline(s *Snapshot, file, pkgPath string) map[string]bool {
	return errorSet(errorsIn(append(s.dirtyByFiles(map[string]bool{file: true}), s.affected(pkgPath)...)))
}

// referencePositions collects up to 10 of obj's non-declaring reference
// positions, for delete_decl/remove_field's "still referenced" Diagnostics
// list — the caller's own declaring identifier is excluded (r.def). A
// reference that falls inside [declStart, declEnd) of declFile is also
// excluded: a recursive call or a self-referential struct type field is a
// reference to obj from within obj's own declaration, not a reference from
// anywhere else, so it must not block deleting that declaration. Pass
// declFile == "" to skip this second filter (remove_field's field-level
// deletes: a field spec doesn't self-reference the way a whole decl can).
// An empty result means genuinely unreferenced.
func referencePositions(s *Snapshot, obj types.Object, declFile string, declStart, declEnd int) []Diagnostic {
	var refs []Diagnostic
	for _, r := range s.references(obj) {
		if r.def {
			continue
		}
		if declFile != "" && r.pos.Filename == declFile && r.pos.Offset >= declStart && r.pos.Offset < declEnd {
			continue
		}
		refs = append(refs, Diagnostic{Pos: r.pos.String()})
		if len(refs) >= 10 {
			break
		}
	}
	return refs
}

// deleteDeclEdit computes delete_decl's edit: the symbol's whole
// declaration range (including its doc comment, via findDeclNode) replaced
// with nothing. Rejects while any non-declaring reference remains.
func deleteDeclEdit(s *Snapshot, pkgPath, sym string) (e edit, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return edit{}, rej0
	}
	filename, decl, doc := s.findDeclNode(p, obj.Name(), sym)
	if filename == "" {
		return edit{}, &Reject{Reason: "declaration not found", Detail: pkgPath + "." + sym}
	}
	start := decl.Pos()
	if doc != nil {
		start = doc.Pos()
	}
	startOff := s.fset.Position(start).Offset
	endOff := s.fset.Position(decl.End()).Offset
	if refs := referencePositions(s, obj, filename, startOff, endOff); len(refs) > 0 {
		return edit{}, &Reject{Reason: "symbol is still referenced", Diagnostics: refs}
	}
	return edit{filename, startOff, endOff - startOff, ""}, nil
}

// deleteDeclOp removes a top-level declaration entirely. Deleting is an API
// change, so its own package's transitive reverse importers join the dirty
// set via ctx.addAffected — a deleted declaration used only through an
// interface elsewhere wouldn't otherwise surface as a broken reference in
// the same file.
type deleteDeclOp struct{}

func (deleteDeclOp) name() string { return "delete_decl" }

func (deleteDeclOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg string `json:"pkg"`
		Sym string `json:"sym"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := deleteDeclEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, e.file, pkg))
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.noteTouched(pkg, sym, true)
	return nil
}

// setDocEdit computes set_doc's edit: text's lines, each rendered with a
// "// " prefix, replacing (or, when sym has none, creating) sym's doc
// comment immediately before its declaration.
func setDocEdit(s *Snapshot, pkgPath, sym, text string) (e edit, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return edit{}, rej0
	}
	filename, decl, doc := s.findDeclNode(p, obj.Name(), sym)
	if filename == "" {
		return edit{}, &Reject{Reason: "declaration not found", Detail: pkgPath + "." + sym}
	}
	var rendered strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		rendered.WriteString("// ")
		rendered.WriteString(line)
		rendered.WriteString("\n")
	}
	declOff := s.fset.Position(decl.Pos()).Offset
	if doc != nil {
		docOff := s.fset.Position(doc.Pos()).Offset
		return edit{filename, docOff, declOff - docOff, rendered.String()}, nil
	}
	return edit{filename, declOff, 0, rendered.String()}, nil
}

// setDocOp replaces or creates a declaration's doc comment. Doc comments
// don't change a package's exported API, so (matching set_body) this
// doesn't join ctx.affectedPkgs.
type setDocOp struct{}

func (setDocOp) name() string { return "set_doc" }

func (setDocOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Sym  string `json:"sym"`
		Text string `json:"text"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := setDocEdit(ctx.s, pkg, sym, a.Text)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.noteTouched(pkg, sym, false)
	return nil
}

// structTypeDecl locates name's declaration and asserts it's a standalone
// struct type, for add_field/remove_field.
func structTypeDecl(s *Snapshot, p *packages.Package, name string) (file string, st *ast.StructType, rej *Reject) {
	filename, decl, _ := s.findDeclNode(p, name, name)
	if filename == "" {
		return "", nil, &Reject{Reason: "declaration not found", Detail: name}
	}
	gd, ok := decl.(*ast.GenDecl)
	if !ok || len(gd.Specs) != 1 {
		return "", nil, &Reject{Reason: "symbol is not a struct type", Detail: nodeKindName(decl)}
	}
	ts, ok := gd.Specs[0].(*ast.TypeSpec)
	if !ok {
		return "", nil, &Reject{Reason: "symbol is not a struct type"}
	}
	st, ok = ts.Type.(*ast.StructType)
	if !ok {
		return "", nil, &Reject{Reason: "symbol is not a struct type", Detail: nodeKindName(ts.Type)}
	}
	return filename, st, nil
}

// addFieldEdit computes add_field's edit: name/type (and optional tag)
// appended to sym's struct field list, right before the closing brace. The
// inserted text always starts with a newline so it can never glue onto a
// preceding field on the same line (e.g. a single-line "struct{ n int }")
// without a separating statement boundary.
func addFieldEdit(s *Snapshot, pkgPath, sym, name, typ, tag string) (e edit, rej *Reject) {
	if !token.IsIdentifier(name) {
		return edit{}, &Reject{Reason: "field name is not a valid identifier", Detail: name}
	}
	if typ == "" {
		return edit{}, &Reject{Reason: "field type is required"}
	}
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return edit{}, rej0
	}
	if _, ok := obj.(*types.TypeName); !ok {
		return edit{}, &Reject{Reason: "symbol is not a type", Detail: objKind(obj)}
	}
	file, st, rej1 := structTypeDecl(s, p, sym)
	if rej1 != nil {
		return edit{}, rej1
	}
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if n.Name == name {
				return edit{}, &Reject{Reason: "field already exists", Detail: name}
			}
		}
	}
	offset := s.fset.Position(st.Fields.Closing).Offset
	line := "\n\t" + name + " " + typ
	if tag != "" {
		line += " `" + tag + "`"
	}
	line += "\n"
	return edit{file, offset, 0, line}, nil
}

// addFieldOp appends a field to a struct type. Adding a field is an API
// change (it can shift unkeyed struct-literal positions and change the
// method-set-adjacent field list reverse importers see), so it joins
// ctx.affectedPkgs.
type addFieldOp struct{}

func (addFieldOp) name() string { return "add_field" }

func (addFieldOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Sym  string `json:"sym"`
		Name string `json:"name"`
		Type string `json:"type"`
		Tag  string `json:"tag"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := addFieldEdit(ctx.s, pkg, sym, a.Name, a.Type, a.Tag)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, e.file, pkg))
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.noteTouched(pkg, sym, false)
	return nil
}

// removeFieldEdit computes remove_field's edit: sym (Type.Field) deleted
// from its struct's field list. Rejects while referenced, and when the
// field shares a multi-name declaration ("a, b int") — removing just one
// name out of such a group needs splitting the spec, out of v1 scope; an
// embedded (anonymous) field is likewise out of v1 scope.
func removeFieldEdit(s *Snapshot, pkgPath, sym string) (e edit, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return edit{}, rej0
	}
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() {
		return edit{}, &Reject{Reason: "symbol is not a field", Detail: objKind(obj)}
	}
	if refs := referencePositions(s, obj, "", 0, 0); len(refs) > 0 {
		return edit{}, &Reject{Reason: "field is still referenced", Diagnostics: refs}
	}
	recv, fname, _ := strings.Cut(sym, ".")
	file, st, rej1 := structTypeDecl(s, p, recv)
	if rej1 != nil {
		return edit{}, rej1
	}
	for _, f := range st.Fields.List {
		idx := -1
		for i, n := range f.Names {
			if n.Name == fname {
				idx = i
				break
			}
		}
		if idx == -1 {
			continue
		}
		if len(f.Names) != 1 {
			return edit{}, &Reject{Reason: "field shares a declaration with other names; remove_field does not support that in v1",
				Detail: fname}
		}
		start := s.fset.Position(f.Pos()).Offset
		end := s.fset.Position(f.End()).Offset
		return edit{file, start, end - start, ""}, nil
	}
	return edit{}, &Reject{Reason: "field declaration not found", Detail: sym}
}

// removeFieldOp deletes a struct field. Same API-change reasoning as
// addFieldOp: joins ctx.affectedPkgs.
type removeFieldOp struct{}

func (removeFieldOp) name() string { return "remove_field" }

func (removeFieldOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg string `json:"pkg"`
		Sym string `json:"sym"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := removeFieldEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, e.file, pkg))
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	// The declaration reshaped is the containing struct type, not the
	// field: sym is "Type.Field", and fields aren't independently viewable.
	recv, _, _ := strings.Cut(sym, ".")
	ctx.noteTouched(pkg, recv, false)
	return nil
}

func init() {
	opRegistry["rename"] = func() patchOp { return renameOp{} }
	opRegistry["set_body"] = func() patchOp { return setBodyOp{} }
	opRegistry["add_param"] = func() patchOp { return addParamOp{} }
	opRegistry["upsert_decl"] = func() patchOp { return upsertDeclOp{} }
	opRegistry["delete_decl"] = func() patchOp { return deleteDeclOp{} }
	opRegistry["set_doc"] = func() patchOp { return setDocOp{} }
	opRegistry["add_field"] = func() patchOp { return addFieldOp{} }
	opRegistry["remove_field"] = func() patchOp { return removeFieldOp{} }
}
