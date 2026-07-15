package snapshot

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// Query dispatches to one of the seven semantic query kinds behind a single
// entry point: the three pre-existing standalone ops (search, inspect,
// refs) keep their own routes elsewhere, and become kinds here too, plus
// the four new call-graph and doc queries. sym and q overlap deliberately:
// kind=="search" reads its fragment from q, falling back to sym, so the
// wire protocol can keep reusing one field the way the standalone search
// op already does (see protocol.Request.Sym).
func (s *Snapshot) Query(kind, pkgPath, sym, q string) (map[string]any, error) {
	switch kind {
	case "search":
		query := q
		if query == "" {
			query = sym
		}
		return s.Search(query)
	case "inspect":
		return s.Inspect(pkgPath, sym)
	case "refs":
		return s.Refs(pkgPath, sym)
	case "callers":
		return s.Callers(pkgPath, sym)
	case "callees":
		return s.Callees(pkgPath, sym)
	case "implementations":
		return s.Implementations(pkgPath, sym)
	case "doc":
		return s.Doc(pkgPath, sym)
	default:
		return nil, &Reject{Reason: "unknown query kind", Detail: kind,
			DidYouMean: []string{"search", "inspect", "refs", "callers", "callees", "implementations", "doc"}}
	}
}

type callerHit struct {
	Pkg     string `json:"pkg"`
	Sym     string `json:"sym"`
	Pos     string `json:"pos"`
	CallPos string `json:"call_pos"`
}

// Callers finds every call-shaped reference to a function or method and
// reports the enclosing function of each call site. Non-call references
// (assignments, arguments passed as values, interface satisfaction) are
// excluded — refs is the query for those.
func (s *Snapshot) Callers(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	_, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	var hits []callerHit
	seen := map[string]bool{}
	s.funcUses(fn, func(p *packages.Package, id *ast.Ident, pos token.Position, call *ast.CallExpr) {
		if call == nil {
			return // not call-shaped; refs is the query for value uses
		}
		callPos := p.Fset.Position(call.Pos())
		if seen[callPos.String()] {
			return
		}
		decl, _ := enclosingFuncOf(p, call.Pos())
		if decl == nil {
			return // call site not inside a named function (e.g. a var initializer)
		}
		seen[callPos.String()] = true
		callerSym := decl.Name.Name
		if recv := recvTypeName(decl); recv != "" {
			callerSym = recv + "." + callerSym
		}
		hits = append(hits, callerHit{
			Pkg: p.PkgPath, Sym: callerSym,
			Pos:     p.Fset.Position(decl.Name.Pos()).String(),
			CallPos: callPos.String(),
		})
	})
	return map[string]any{"status": "ok", "symbol": pkgPath + "." + sym,
		"count": len(hits), "callers": hits, "load_ms": ms}, nil
}

// enclosingFuncOf finds the top-level FuncDecl in p's syntax whose range
// contains pos (a call site, typically nested inside its body), and the
// filename it lives in. Only top-level declarations are considered:
// anonymous closures are not addressable symbols, so a call inside one is
// attributed to the named function that encloses it.
func enclosingFuncOf(p *packages.Package, pos token.Pos) (*ast.FuncDecl, string) {
	for _, f := range p.Syntax {
		if pos < f.Pos() || pos > f.End() {
			continue
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos <= fd.End() {
				return fd, p.Fset.Position(f.Pos()).Filename
			}
		}
	}
	return nil, ""
}

type calleeHit struct {
	Pkg     string `json:"pkg"`
	Sym     string `json:"sym"`
	CallPos string `json:"call_pos"`
}

// Callees walks a function's body for call expressions and resolves each
// one's target via the type checker. Type conversions, builtin calls, and
// calls through a function value all resolve to something other than a
// *types.Func and are skipped. A call through an interface method resolves
// to the interface's method — that IS the static callee; dynamic dispatch
// is a runtime fact this tier does not track.
func (s *Snapshot) Callees(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	decl, _ := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	var hits []calleeHit
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id := calleeIdent(call.Fun)
		if id == nil {
			return true
		}
		target, ok := p.TypesInfo.Uses[id]
		if !ok {
			return true
		}
		tfn, ok := target.(*types.Func)
		if !ok {
			return true // type conversion, builtin, or a call through a value
		}
		calleeSym := tfn.Name()
		if recvVar := tfn.Signature().Recv(); recvVar != nil {
			if recv := namedTypeName(recvVar.Type()); recv != "" {
				calleeSym = recv + "." + calleeSym
			}
		}
		calleePkg := ""
		if tfn.Pkg() != nil {
			calleePkg = tfn.Pkg().Path()
		}
		hits = append(hits, calleeHit{Pkg: calleePkg, Sym: calleeSym,
			CallPos: p.Fset.Position(call.Pos()).String()})
		return true
	})
	return map[string]any{"status": "ok", "symbol": pkgPath + "." + sym,
		"count": len(hits), "callees": hits, "load_ms": ms}, nil
}

// calleeIdent unwraps a call expression's Fun down to the identifier that
// names the callee: itself for a plain call, the selector's Sel for a
// method or qualified call, through any number of parens.
func calleeIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.SelectorExpr:
			return x.Sel
		case *ast.Ident:
			return x
		default:
			return nil
		}
	}
}

// namedTypeName returns a receiver type's name, unwrapping one pointer
// level, or "" for a type with no name (e.g. an unnamed interface).
func namedTypeName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return n.Obj().Name()
	}
	return ""
}

type implHit struct {
	Pkg string `json:"pkg"`
	Sym string `json:"sym"`
	Pos string `json:"pos"`
}

// Implementations works in both directions. For an interface, it scans the
// workspace for named concrete types whose value or pointer type implements
// it. For a concrete type, it scans the workspace for interfaces the type
// (by value or by pointer) satisfies. Only primary package variants are
// scanned so a symbol declared once doesn't get reported once per test
// variant.
func (s *Snapshot) Implementations(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	_, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil, &Reject{Reason: "symbol is not a type", Detail: objKind(obj)}
	}
	self := pkgPath + "." + sym
	iface, isIface := tn.Type().Underlying().(*types.Interface)
	named, _ := tn.Type().(*types.Named)

	var hits []implHit
	for _, p := range s.workspacePackages() {
		if p.ID != p.PkgPath || p.Types == nil {
			continue // primary variants only
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if p.PkgPath+"."+name == self {
				continue
			}
			candTN, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			candIface, candIsIface := candTN.Type().Underlying().(*types.Interface)
			switch {
			case isIface && !candIsIface:
				if types.Implements(candTN.Type(), iface) || types.Implements(types.NewPointer(candTN.Type()), iface) {
					hits = append(hits, implHit{p.PkgPath, name, p.Fset.Position(candTN.Pos()).String()})
				}
			case !isIface && candIsIface && named != nil:
				if types.Implements(named, candIface) || types.Implements(types.NewPointer(named), candIface) {
					hits = append(hits, implHit{p.PkgPath, name, p.Fset.Position(candTN.Pos()).String()})
				}
			}
		}
	}
	if isIface {
		return map[string]any{"status": "ok", "symbol": self, "direction": "interface_to_types",
			"count": len(hits), "types": hits, "load_ms": ms}, nil
	}
	return map[string]any{"status": "ok", "symbol": self, "direction": "type_to_interfaces",
		"count": len(hits), "interfaces": hits, "load_ms": ms}, nil
}

// Doc returns a declaration's doc comment as plain text (comment markers
// stripped), or an empty string when it has none.
func (s *Snapshot) Doc(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	_, decl, doc := s.findDeclNode(p, obj.Name(), sym)
	if decl == nil {
		return nil, &Reject{Reason: "declaration not found", Detail: pkgPath + "." + sym}
	}
	text := ""
	if doc != nil {
		text = doc.Text()
	}
	return map[string]any{"status": "ok", "symbol": pkgPath + "." + sym,
		"doc": text, "load_ms": ms}, nil
}
