package snapshot

import (
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

// AddParam appends a parameter to a function or method and rewrites every
// call site to pass defaultExpr explicitly. References to the function as a
// value (assignments, arguments, interface method sets) cannot be repaired
// with a default and are rejected with their positions. API changes, so the
// dirty set includes the target package's transitive reverse importers.
func (s *Snapshot) AddParam(pkgPath, sym, name, typ, defaultExpr string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !token.IsIdentifier(name) {
		return nil, &Reject{Reason: "parameter name is not a valid identifier", Detail: name}
	}
	if typ == "" {
		return nil, &Reject{Reason: "parameter type is required"}
	}
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, declFile := findFuncDecl(p, fn)
	if decl == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}

	calls, valueUses := s.callSites(fn)
	if len(valueUses) > 0 {
		return nil, &Reject{Reason: "function is used as a value; add_param cannot repair those sites",
			Diagnostics: valueUses}
	}
	if len(calls) > 0 && defaultExpr == "" {
		return nil, &Reject{Reason: "default expression required",
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
	type insertion struct {
		file   string
		offset int
		text   string
	}
	var edits []insertion
	if sep == "" && params.NumFields() > 0 {
		// Inserting before the variadic parameter.
		edits = append(edits, insertion{declFile, declInsert, name + " " + typ + ", "})
	} else {
		edits = append(edits, insertion{declFile, declInsert, sep + name + " " + typ})
	}
	for _, c := range calls {
		argSep := ", "
		if len(c.call.Args) == 0 {
			argSep = ""
		}
		if c.call.Ellipsis.IsValid() {
			return nil, &Reject{Reason: "call site spreads arguments with ...; cannot append a default",
				Diagnostics: []Diagnostic{{Pos: c.pos.String()}}}
		}
		edits = append(edits, insertion{c.pos.Filename,
			s.fset.Position(c.call.Rparen).Offset, argSep + defaultExpr})
	}

	byFile := map[string][]insertion{}
	editedFiles := map[string]bool{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
		editedFiles[e.file] = true
	}
	preDirty := append(s.dirtyByFiles(editedFiles), s.affected(pkgPath)...)
	if diags := errorsIn(preDirty); len(diags) > 0 {
		return nil, &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
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
			out = append(append(append([]byte{}, out[:e.offset]...), e.text...), out[e.offset:]...)
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
		return nil, &Reject{Reason: "add_param does not typecheck", Diagnostics: diags}
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
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym,
		"param": name + " " + typ, "callers_updated": len(calls),
		"files": len(byFile), "load_ms": ms, "packages_rechecked": n,
	}, nil
}

type callSite struct {
	call *ast.CallExpr
	pos  token.Position
}

// callSites splits every reference to fn into direct calls and value uses.
// The definition itself is neither.
func (s *Snapshot) callSites(fn *types.Func) ([]callSite, []Diagnostic) {
	key := s.objKey(fn)
	var calls []callSite
	var values []Diagnostic
	seenCall := map[string]bool{}
	seenVal := map[string]bool{}
	for _, p := range s.pkgs {
		if p.TypesInfo == nil {
			continue
		}
		for id, o := range p.TypesInfo.Uses {
			if o == nil || o.Name() != fn.Name() || s.objKey(o) != key {
				continue
			}
			pos := p.Fset.Position(id.Pos())
			if !strings.HasPrefix(pos.Filename, s.dir+string(os.PathSeparator)) {
				continue
			}
			call := enclosingCall(fileFor(p, id.Pos()), id)
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
		}
	}
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
