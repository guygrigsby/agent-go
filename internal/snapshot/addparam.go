package snapshot

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
)

// addParamEdits computes add_param's edits without touching disk: symbol
// lookup, the value-use/call-site scan, the declaration and call-site
// insertion points, and the pre-existing-diagnostics baseline capture (the
// dirty set's current errors, filtered against the caller's post-splice
// retypecheck so only NEW diagnostics reject), exactly as AddParam has
// always done inline.
func addParamEdits(s *Snapshot, pkgPath, sym, name, typ, defaultExpr string) (edits []edit, callersUpdated int, baseline map[string]bool, rej *Reject) {
	if !token.IsIdentifier(name) {
		return nil, 0, nil, &Reject{Reason: "parameter name is not a valid identifier", Detail: name}
	}
	if typ == "" {
		return nil, 0, nil, &Reject{Reason: "parameter type is required"}
	}
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return nil, 0, nil, rej0
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, 0, nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, declFile := findFuncDecl(p, fn)
	if decl == nil {
		return nil, 0, nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}

	calls, valueUses := s.callSites(fn)
	if len(valueUses) > 0 {
		return nil, 0, nil, &Reject{Reason: "function is used as a value; add_param cannot repair those sites",
			Diagnostics: valueUses}
	}
	if len(calls) > 0 && defaultExpr == "" {
		return nil, 0, nil, &Reject{Reason: "default expression required",
			Detail: fmt.Sprintf("%d call sites need an argument", len(calls))}
	}

	// Parameters are declared in the function body's scope, so a top-level
	// `name := ...` or `var name ...` in the body collides with the new
	// parameter ("no new variables on left side of :=" / redeclared). When
	// the local is exactly `name := <defaultExpr>`, the parameter supersedes
	// it: delete the declaration and every path — old callers passing the
	// default, the body reading the parameter — behaves as before. Any other
	// same-named body declaration is rejected with its position instead of
	// the raw typecheck error.
	if decl.Body != nil {
		for _, st := range decl.Body.List {
			pos, promote := bodyDeclCollision(st, name, defaultExpr)
			if !pos.IsValid() {
				continue
			}
			if promote == nil {
				return nil, 0, nil, &Reject{Reason: "parameter name collides with a declaration in the function body",
					Detail: name + " is already declared there and parameters share the body scope; rename or delete the local, or make default match its initializer so add_param can promote it",
					Diagnostics: []Diagnostic{{Pos: s.fset.Position(pos).String(),
						Msg: name + " declared here"}}}
			}
			start := s.fset.Position(promote.Pos()).Offset
			end := s.fset.Position(promote.End()).Offset
			start, end = expandToLine(declFile, start, end)
			edits = append(edits, edit{declFile, start, end - start, ""})
		}
	}

	// Declaration edit: append before the closing paren, or before a final
	// variadic parameter (nothing may follow a variadic).
	params := decl.Type.Params
	declInsert := s.fset.Position(params.Closing).Offset
	sep := ", "
	variadic := false
	if params.NumFields() == 0 {
		sep = ""
	} else if last := params.List[len(params.List)-1]; last != nil {
		if _, variadic = last.Type.(*ast.Ellipsis); variadic {
			declInsert = s.fset.Position(last.Pos()).Offset
			sep = ""
		}
	}
	if variadic {
		// Inserting before the variadic parameter.
		edits = append(edits, edit{declFile, declInsert, 0, name + " " + typ + ", "})
	} else {
		// A multiline parameter list carries a trailing comma before the
		// closing paren; appending ", x T" after it would read ", ,".
		if declSrc, err := os.ReadFile(declFile); err == nil {
			i := declInsert - 1
			for i >= 0 && (declSrc[i] == ' ' || declSrc[i] == '\t' || declSrc[i] == '\n' || declSrc[i] == '\r') {
				i--
			}
			if i >= 0 && declSrc[i] == ',' {
				sep = ""
				declInsert = i + 1
				edits = append(edits, edit{declFile, declInsert, 0, " " + name + " " + typ + ","})
			} else {
				edits = append(edits, edit{declFile, declInsert, 0, sep + name + " " + typ})
			}
		} else {
			edits = append(edits, edit{declFile, declInsert, 0, sep + name + " " + typ})
		}
	}
	// fixedArgs is where the new argument lands at every call site: after
	// the existing fixed parameters, before any variadic tail — which is
	// what lets spread sites f(args...) take a default too.
	fixedArgs := params.NumFields()
	if variadic {
		fixedArgs = 0
		for _, f := range params.List[:len(params.List)-1] {
			fixedArgs += max(len(f.Names), 1)
		}
	}
	for _, c := range calls {
		switch {
		case len(c.call.Args) > fixedArgs:
			// Insert before the first variadic argument (a spread slice or
			// the first of the values).
			at := s.fset.Position(c.call.Args[fixedArgs].Pos()).Offset
			edits = append(edits, edit{c.pos.Filename, at, 0, defaultExpr + ", "})
		case len(c.call.Args) == 0:
			edits = append(edits, edit{c.pos.Filename,
				s.fset.Position(c.call.Rparen).Offset, 0, defaultExpr})
		default:
			edits = append(edits, edit{c.pos.Filename,
				s.fset.Position(c.call.Rparen).Offset, 0, ", " + defaultExpr})
		}
	}

	byFile := map[string][]edit{}
	editedFiles := map[string]bool{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	baseline = errorSet(errorsIn(preDirty))
	return edits, len(calls), baseline, nil
}

// AddParam appends a parameter to a function or method and rewrites every
// call site to pass defaultExpr explicitly. References to the function as a
// value (assignments, arguments, interface method sets) cannot be repaired
// with a default and are rejected with their positions. API changes, so the
// dirty set includes the target package's transitive reverse importers.
func (s *Snapshot) AddParam(pkgPath, sym, name, typ, defaultExpr string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	edits, callersUpdated, baseline, rej := addParamEdits(s, pkgPath, sym, name, typ, defaultExpr)
	if rej != nil {
		s.sugarRepairs(rej, "add_param",
			map[string]any{"pkg": pkgPath, "sym": sym, "name": name,
				"type": typ, "default": defaultExpr}, s.resolvesToFunc)
		return nil, rej
	}

	byFile := map[string][]edit{}
	editedFiles := map[string]bool{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	originals := map[string][]byte{}
	for file, fedits := range byFile {
		src, err := os.ReadFile(file)
		if err != nil {
			s.rollback(originals)
			return nil, err
		}
		originals[file] = src
		sort.Slice(fedits, func(i, j int) bool { return fedits[i].offset > fedits[j].offset })
		out := src
		for _, e := range fedits {
			out = append(append(append([]byte{}, out[:e.offset]...), e.text...), out[e.offset+e.length:]...)
		}
		// The new parameter's type (and the default expression at call
		// sites) can name a package the file does not import yet.
		fixed, ferr := imports.Process(file, out, nil)
		if ferr != nil {
			s.rollback(originals)
			return nil, &Reject{Reason: "add_param result does not parse", Detail: file + ": " + ferr.Error()}
		}
		if err := os.WriteFile(file, fixed, 0o644); err != nil {
			s.rollback(originals)
			return nil, err
		}
	}

	dirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	diags, n, err := s.retypecheck(dirty, baseline)
	if err != nil {
		s.rollback(originals)
		return nil, err
	}
	if len(diags) > 0 {
		s.rollback(originals)
		return nil, diagnosticRepairs(&Reject{Reason: "add_param does not typecheck", Diagnostics: diags})
	}
	// Sanity: the new signature must actually carry the parameter.
	if _, obj, rej := s.findObject(pkgPath, sym); rej != nil || !hasParam(obj, name) {
		s.rollback(originals)
		s.retypecheck(dirty, baseline)
		return nil, &Reject{Reason: "parameter missing after edit", Detail: sym}
	}
	for file := range editedFiles {
		s.noteWrite(file)
	}
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
	}
	sort.Strings(files)
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym,
		"param": name + " " + typ, "callers_updated": callersUpdated,
		"files": files, "load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(pkgPath, sym),
	}, baseline)
	// Call-site files were edited too, but the one declaration reshaped is
	// the target function; its fresh view is what the next edit needs.
	s.attachView(res, pkgPath, sym)
	return res, nil
}

// addParamOp is add_param's composable form: same addParamEdits core,
// applied to ctx.src through the decl-op ledger instead of straight to
// disk, with the post-edit "parameter actually landed" sanity check
// deferred to a postCheck run once at end-of-list.
type addParamOp struct{}

func (addParamOp) name() string { return "add_param" }

func (addParamOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg     string `json:"pkg"`
		Sym     string `json:"sym"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		Default string `json:"default"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	edits, _, baseline, rej := addParamEdits(ctx.s, pkg, sym, a.Name, a.Type, a.Default)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(baseline)
	if rej := ctx.applyDeclEdits(edits); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.noteTouched(pkg, sym, false)
	ctx.postChecks = append(ctx.postChecks, func() *Reject {
		if _, obj, rej := ctx.s.findObject(pkg, sym); rej != nil || !hasParam(obj, a.Name) {
			return &Reject{Reason: "parameter missing after edit", Detail: sym}
		}
		return nil
	})
	return nil
}

// bodyDeclCollision reports whether stmt declares name (a collision with a
// new parameter of that name: parameters live in the body block's scope).
// When the statement is exactly `name := <expr>` with expr equal to
// defaultExpr, it is also returned as promotable: deleting it and letting
// the parameter carry the value preserves behavior at every existing call
// site, since they all receive defaultExpr.
func bodyDeclCollision(stmt ast.Stmt, name, defaultExpr string) (token.Pos, *ast.AssignStmt) {
	switch st := stmt.(type) {
	case *ast.AssignStmt:
		if st.Tok != token.DEFINE {
			return token.NoPos, nil
		}
		for _, lhs := range st.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name {
				continue
			}
			if len(st.Lhs) == 1 && len(st.Rhs) == 1 && exprMatches(st.Rhs[0], defaultExpr) {
				return id.Pos(), st
			}
			return id.Pos(), nil
		}
	case *ast.DeclStmt:
		gd, ok := st.Decl.(*ast.GenDecl)
		if !ok || gd.Tok == token.IMPORT {
			return token.NoPos, nil
		}
		for _, sp := range gd.Specs {
			switch sp := sp.(type) {
			case *ast.ValueSpec:
				for _, id := range sp.Names {
					if id.Name == name {
						return id.Pos(), nil
					}
				}
			case *ast.TypeSpec:
				if sp.Name.Name == name {
					return sp.Name.Pos(), nil
				}
			}
		}
	}
	return token.NoPos, nil
}

// exprMatches reports whether e and the (parseable) expression source src
// denote the same expression text, ignoring formatting.
func exprMatches(e ast.Expr, src string) bool {
	d, err := parser.ParseExpr(src)
	if err != nil {
		return false
	}
	return types.ExprString(e) == types.ExprString(d)
}

// expandToLine widens a deletion of src[start:end) in file to swallow the
// whole line — leading indentation and the trailing newline — when the span
// is alone on its line, so removing a statement does not leave a blank line
// behind. Any read or bounds trouble leaves the span untouched.
func expandToLine(file string, start, end int) (int, int) {
	src, err := os.ReadFile(file)
	if err != nil || start < 0 || end > len(src) {
		return start, end
	}
	ls := start
	for ls > 0 && (src[ls-1] == ' ' || src[ls-1] == '\t') {
		ls--
	}
	le := end
	for le < len(src) && (src[le] == ' ' || src[le] == '\t' || src[le] == '\r') {
		le++
	}
	if (ls == 0 || src[ls-1] == '\n') && le < len(src) && src[le] == '\n' {
		return ls, le + 1
	}
	return start, end
}

type callSite struct {
	call *ast.CallExpr
	pos  token.Position
}

// funcUses walks every workspace package's Uses map for identifiers
// resolving to fn, resolving each to its enclosing CallExpr when the
// reference is call-shaped (nil otherwise, meaning a value use). This is
// the shared core behind callSites (add_param's call/value-use split) and
// Callers (the query tier's call-graph edges).
func (s *Snapshot) funcUses(fn *types.Func, visit func(p *packages.Package, id *ast.Ident, pos token.Position, call *ast.CallExpr)) {
	key := s.objKey(fn)
	for _, p := range s.pkgs {
		if p.TypesInfo == nil {
			continue
		}
		// Uses is a map; collect and sort so visit order (and thus Callers
		// output and add_param edit order) is deterministic.
		var ids []*ast.Ident
		for id, o := range p.TypesInfo.Uses {
			if o == nil || o.Name() != fn.Name() || s.objKey(o) != key {
				continue
			}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].Pos() < ids[j].Pos() })
		for _, id := range ids {
			pos := p.Fset.Position(id.Pos())
			call := enclosingCall(fileFor(p, id.Pos()), id)
			visit(p, id, pos, call)
		}
	}
}

// callSites splits every reference to fn into direct calls and value uses.
// The definition itself is neither.
func (s *Snapshot) callSites(fn *types.Func) ([]callSite, []Diagnostic) {
	var calls []callSite
	var values []Diagnostic
	seenCall := map[string]bool{}
	seenVal := map[string]bool{}
	s.funcUses(fn, func(p *packages.Package, id *ast.Ident, pos token.Position, call *ast.CallExpr) {
		if !strings.HasPrefix(pos.Filename, s.dir+string(os.PathSeparator)) {
			return
		}
		if call != nil {
			if k := pos.String(); !seenCall[k] {
				seenCall[k] = true
				calls = append(calls, callSite{call, pos})
			}
		} else if k := pos.String(); !seenVal[k] {
			seenVal[k] = true
			values = append(values, Diagnostic{Pos: pos.String(),
				Msg: "function referenced as a value"})
		}
	})
	return calls, values
}

func fileFor(p *packages.Package, pos token.Pos) *ast.File {
	for _, f := range p.Syntax {
		if f.Pos() <= pos && pos <= f.End() {
			return f
		}
	}
	return nil
}

// enclosingCall returns the CallExpr that invokes id (directly or through a
// selector), or nil when id is a value reference.
func enclosingCall(file *ast.File, id *ast.Ident) *ast.CallExpr {
	if file == nil {
		return nil
	}
	path, _ := astutil.PathEnclosingInterval(file, id.Pos(), id.End())
	var prev ast.Node = id
	for _, n := range path {
		switch e := n.(type) {
		case *ast.Ident:
		case *ast.SelectorExpr:
			if e.Sel != prev {
				return nil
			}
		case *ast.ParenExpr:
		case *ast.CallExpr:
			if e.Fun == prev {
				return e
			}
			return nil
		default:
			return nil
		}
		if n != id {
			prev = n
		}
	}
	return nil
}

func hasParam(obj types.Object, name string) bool {
	fn, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	params := fn.Signature().Params()
	for v := range params.Variables() {
		if v.Name() == name {
			return true
		}
	}
	return false
}
