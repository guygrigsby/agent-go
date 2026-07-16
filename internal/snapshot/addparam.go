package snapshot

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

// addParamEdits computes add_param's edits without touching disk: symbol
// lookup, the value-use/call-site scan, the declaration and call-site
// insertion points, and the pre-existing-errors preflight, exactly as
// AddParam has always done inline.
func addParamEdits(s *Snapshot, pkgPath, sym, name, typ, defaultExpr string) (edits []edit, callersUpdated int, rej *Reject) {
	if !token.IsIdentifier(name) {
		return nil, 0, &Reject{Reason: "parameter name is not a valid identifier", Detail: name}
	}
	if typ == "" {
		return nil, 0, &Reject{Reason: "parameter type is required"}
	}
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return nil, 0, rej0
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, 0, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, declFile := findFuncDecl(p, fn)
	if decl == nil {
		return nil, 0, &Reject{Reason: "function declaration not found", Detail: sym}
	}

	calls, valueUses := s.callSites(fn)
	if len(valueUses) > 0 {
		return nil, 0, &Reject{Reason: "function is used as a value; add_param cannot repair those sites",
			Diagnostics: valueUses}
	}
	if len(calls) > 0 && defaultExpr == "" {
		return nil, 0, &Reject{Reason: "default expression required",
			Detail: fmt.Sprintf("%d call sites need an argument", len(calls))}
	}

	// Declaration edit: append before the closing paren, or before a final
	// variadic parameter (nothing may follow a variadic).
	params := decl.Type.Params
	declInsert := s.fset.Position(params.Closing).Offset
	sep := ", "
	if params.NumFields() == 0 {
		sep = ""
	} else if last := params.List[len(params.List)-1]; last != nil {
		if _, variadic := last.Type.(*ast.Ellipsis); variadic {
			declInsert = s.fset.Position(last.Pos()).Offset
			sep = ""
		}
	}
	if sep == "" && params.NumFields() > 0 {
		// Inserting before the variadic parameter.
		edits = append(edits, edit{declFile, declInsert, 0, name + " " + typ + ", "})
	} else {
		edits = append(edits, edit{declFile, declInsert, 0, sep + name + " " + typ})
	}
	for _, c := range calls {
		argSep := ", "
		if len(c.call.Args) == 0 {
			argSep = ""
		}
		if c.call.Ellipsis.IsValid() {
			return nil, 0, &Reject{Reason: "call site spreads arguments with ...; cannot append a default",
				Diagnostics: []Diagnostic{{Pos: c.pos.String()}}}
		}
		edits = append(edits, edit{c.pos.Filename,
			s.fset.Position(c.call.Rparen).Offset, 0, argSep + defaultExpr})
	}

	byFile := map[string][]edit{}
	editedFiles := map[string]bool{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	if diags := errorsIn(preDirty); len(diags) > 0 {
		return nil, 0, &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
	}
	return edits, len(calls), nil
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
	edits, callersUpdated, rej := addParamEdits(s, pkgPath, sym, name, typ, defaultExpr)
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
		if err := os.WriteFile(file, out, 0o644); err != nil {
			s.rollback(originals)
			return nil, err
		}
	}

	dirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	diags, n, err := s.retypecheck(dirty)
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
		s.retypecheck(dirty)
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
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym,
		"param": name + " " + typ, "callers_updated": callersUpdated,
		"files": files, "load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(pkgPath, sym),
	}, nil
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
	if err := json.Unmarshal(raw, &a); err != nil {
		return &Reject{Reason: "malformed op args", Detail: err.Error()}
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	sym := orDefault(a.Sym, ctx.sym)
	edits, _, rej := addParamEdits(ctx.s, pkg, sym, a.Name, a.Type, a.Default)
	if rej != nil {
		return rej
	}
	if rej := ctx.applyDeclEdits(edits); rej != nil {
		return rej
	}
	ctx.addAffected(pkg)
	ctx.postChecks = append(ctx.postChecks, func() *Reject {
		if _, obj, rej := ctx.s.findObject(pkg, sym); rej != nil || !hasParam(obj, a.Name) {
			return &Reject{Reason: "parameter missing after edit", Detail: sym}
		}
		return nil
	})
	return nil
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
		for id, o := range p.TypesInfo.Uses {
			if o == nil || o.Name() != fn.Name() || s.objKey(o) != key {
				continue
			}
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
