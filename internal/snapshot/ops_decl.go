package snapshot

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	file, lbrace, rbrace, rej := setBodyEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	return ctx.applyDeclEdits([]edit{{file, lbrace, rbrace - lbrace + 1, "{\n" + a.Body + "\n}"}})
}

// upsertDeclOp is upsert_decl's composable form: same upsertDeclEdit core
// (locate an existing declaration to replace, or an existing agent.go to
// append to), applied through the decl-op ledger. Creating a brand-new file
// or package is a documented v1 ceiling here — see upsertDeclEdit.
type upsertDeclOp struct{}

func (upsertDeclOp) name() string { return "upsert_decl" }

func (upsertDeclOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	name, sym, rej := parseDeclText(a.Text)
	if rej != nil {
		return rej
	}
	file, start, end, _, needsCreate, rej := upsertDeclEdit(ctx.s, pkg, name, sym)
	if rej != nil {
		return rej
	}
	if needsCreate {
		return &Reject{Reason: "upsert_decl cannot create a new file in a composable patch",
			Detail: "agent.go does not exist yet in " + pkg + "; use the single-op upsert_decl call"}
	}
	if rej := preexistingErrors(ctx.s, file, pkg); rej != nil {
		return rej
	}
	prefix := ""
	if start > 0 {
		prefix = "\n"
	}
	text := prefix + strings.TrimSpace(a.Text) + "\n"
	if rej := ctx.applyDeclEdits([]edit{{file, start, end - start, text}}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.postChecks = append(ctx.postChecks, func() *Reject {
		if _, _, rej := ctx.s.findObject(pkg, sym); rej != nil {
			return &Reject{Reason: "declaration missing after edit", Detail: sym}
		}
		return nil
	})
	return nil
}

// preexistingErrors runs the same "affected packages have pre-existing
// errors" preflight every decl op's core performs before computing its
// edit: the dirty set is file's own compiling packages plus pkgPath's
// transitive reverse importers.
func preexistingErrors(s *Snapshot, file, pkgPath string) *Reject {
	if diags := errorsIn(append(s.dirtyByFiles(map[string]bool{file: true}), s.affected(pkgPath)...)); len(diags) > 0 {
		return &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
	}
	return nil
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
	if rej := preexistingErrors(s, filename, pkgPath); rej != nil {
		return edit{}, rej
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := deleteDeclEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := setDocEdit(ctx.s, pkg, sym, a.Text)
	if rej != nil {
		return rej
	}
	return ctx.applyDeclEdits([]edit{e})
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
	if rej := preexistingErrors(s, file, pkgPath); rej != nil {
		return edit{}, rej
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := addFieldEdit(ctx.s, pkg, sym, a.Name, a.Type, a.Tag)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
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
		if rej := preexistingErrors(s, file, pkgPath); rej != nil {
			return edit{}, rej
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	e, rej := removeFieldEdit(ctx.s, pkg, sym)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
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
