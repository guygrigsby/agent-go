// Package snapshot holds a typechecked view of one Go workspace and answers
// semantic queries and validated mutations against it.
package snapshot

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/packages"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
	packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule

// Reject is a structured refusal: the operation was understood but the
// workspace or the edit does not support it. It is data for the agent, not
// an internal failure.
type Reject struct {
	Reason      string       `json:"reason"`
	Detail      string       `json:"detail,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

func (r *Reject) Error() string { return r.Reason }

type Diagnostic struct {
	Pos string `json:"pos"`
	Msg string `json:"msg"`
}

type Snapshot struct {
	dir string

	mu     sync.Mutex
	pkgs   []*packages.Package
	fset   *token.FileSet
	mtimes map[string]time.Time
	stale  bool
}

func New(dir string) *Snapshot {
	return &Snapshot{dir: dir, stale: true}
}

// load (re)typechecks the whole workspace. Caller holds mu.
func (s *Snapshot) load() (int64, error) {
	cfg := &packages.Config{Mode: loadMode, Dir: s.dir, Tests: true}
	start := time.Now()
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return 0, err
	}
	s.pkgs = pkgs
	s.fset = cfg.Fset
	if s.fset == nil && len(pkgs) > 0 {
		s.fset = pkgs[0].Fset
	}
	s.mtimes = map[string]time.Time{}
	for _, p := range pkgs {
		for _, f := range p.CompiledGoFiles {
			if fi, err := os.Stat(f); err == nil {
				s.mtimes[f] = fi.ModTime()
			}
		}
	}
	s.stale = false
	return time.Since(start).Milliseconds(), nil
}

// ensureFresh reloads if a mutation was accepted or a file changed on disk.
// Returns reload cost in ms (0 when the snapshot was already fresh).
func (s *Snapshot) ensureFresh() (int64, error) {
	if !s.stale {
		for f, t := range s.mtimes {
			fi, err := os.Stat(f)
			if err != nil || !fi.ModTime().Equal(t) {
				s.stale = true
				break
			}
		}
	}
	if s.stale {
		return s.load()
	}
	return 0, nil
}

func (s *Snapshot) errors() []Diagnostic {
	var diags []Diagnostic
	seen := map[string]bool{}
	packages.Visit(s.pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			if key := e.Pos + e.Msg; !seen[key] {
				seen[key] = true
				diags = append(diags, Diagnostic{Pos: e.Pos, Msg: e.Msg})
			}
		}
	})
	return diags
}

// primary returns the non-test variant of a package.
func (s *Snapshot) primary(pkgPath string) *packages.Package {
	var fallback *packages.Package
	for _, p := range s.pkgs {
		if p.PkgPath != pkgPath || p.Types == nil {
			continue
		}
		if p.ID == p.PkgPath {
			return p
		}
		if fallback == nil {
			fallback = p
		}
	}
	return fallback
}

func (s *Snapshot) findObject(pkgPath, sym string) (*packages.Package, types.Object, *Reject) {
	p := s.primary(pkgPath)
	if p == nil {
		return nil, nil, &Reject{Reason: "package not found", Detail: pkgPath}
	}
	scope := p.Types.Scope()
	recv, name, isSel := strings.Cut(sym, ".")
	if !isSel {
		obj := scope.Lookup(sym)
		if obj == nil {
			return nil, nil, &Reject{Reason: "symbol not found", Detail: pkgPath + "." + sym}
		}
		return p, obj, nil
	}
	recvObj := scope.Lookup(recv)
	if recvObj == nil {
		return nil, nil, &Reject{Reason: "receiver type not found", Detail: pkgPath + "." + recv}
	}
	obj, _, _ := types.LookupFieldOrMethod(recvObj.Type(), true, p.Types, name)
	if obj == nil {
		return nil, nil, &Reject{Reason: "method or field not found", Detail: pkgPath + "." + sym}
	}
	return p, obj, nil
}

func objKind(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		if o.Signature().Recv() != nil {
			return "method"
		}
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Var:
		if o.IsField() {
			return "field"
		}
		return "var"
	case *types.Const:
		return "const"
	default:
		return fmt.Sprintf("%T", obj)
	}
}

func (s *Snapshot) Status() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	files := 0
	for _, p := range s.pkgs {
		files += len(p.CompiledGoFiles)
	}
	return map[string]any{
		"status": "ok", "packages": len(s.pkgs), "files": files,
		"errors": s.errors(), "load_ms": ms,
	}, nil
}

func (s *Snapshot) Inspect(pkgPath, sym string) (map[string]any, error) {
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
	return map[string]any{
		"status": "ok", "name": obj.Name(), "kind": objKind(obj),
		"type":     types.TypeString(obj.Type(), types.RelativeTo(p.Types)),
		"exported": obj.Exported(), "pos": s.fset.Position(obj.Pos()).String(),
		"pkg": pkgPath, "load_ms": ms,
	}, nil
}

func (s *Snapshot) Refs(pkgPath, sym string) (map[string]any, error) {
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
	type ref struct {
		Pos string `json:"pos"`
		Pkg string `json:"pkg"`
		Def bool   `json:"def,omitempty"`
	}
	defPos := s.fset.Position(obj.Pos()).String()
	var refs []ref
	seen := map[string]bool{}
	for _, p := range s.pkgs {
		if p.TypesInfo == nil {
			continue
		}
		add := func(id *ast.Ident, o types.Object, def bool) {
			// Identity by defining position: object pointers differ across
			// test-variant packages.
			if o == nil || o.Name() != obj.Name() || !o.Pos().IsValid() ||
				p.Fset.Position(o.Pos()).String() != defPos {
				return
			}
			pos := p.Fset.Position(id.Pos()).String()
			if !seen[pos] {
				seen[pos] = true
				refs = append(refs, ref{Pos: pos, Pkg: p.PkgPath, Def: def})
			}
		}
		for id, o := range p.TypesInfo.Defs {
			add(id, o, true)
		}
		for id, o := range p.TypesInfo.Uses {
			add(id, o, false)
		}
	}
	return map[string]any{
		"status": "ok", "symbol": pkgPath + "." + sym,
		"count": len(refs), "refs": refs, "load_ms": ms,
	}, nil
}

// SetBody replaces a function's body and validates by re-typechecking only
// the target package against the in-memory dependency graph. A body edit
// cannot change the package's exported API, so importers are unaffected.
func (s *Snapshot) SetBody(pkgPath, sym, body string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	if diags := s.errors(); len(diags) > 0 {
		return nil, &Reject{Reason: "workspace has pre-existing errors", Diagnostics: diags}
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, filename := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	lbrace := p.Fset.Position(decl.Body.Lbrace).Offset
	rbrace := p.Fset.Position(decl.Body.Rbrace).Offset
	var buf strings.Builder
	buf.Write(src[:lbrace])
	buf.WriteString("{\n")
	buf.WriteString(body)
	buf.WriteString("\n}")
	buf.Write(src[rbrace+1:])
	formatted, err := format.Source([]byte(buf.String()))
	if err != nil {
		return nil, &Reject{Reason: "new body does not parse", Detail: err.Error()}
	}

	start := time.Now()
	diags, rej := s.checkPackage(p, filename, formatted)
	checkMS := time.Since(start).Milliseconds()
	if rej != nil {
		return nil, rej
	}
	if len(diags) > 0 {
		return nil, &Reject{Reason: "edit does not typecheck", Diagnostics: diags}
	}
	if err := os.WriteFile(filename, formatted, 0o644); err != nil {
		return nil, err
	}
	s.stale = true
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "file": filename,
		"load_ms": ms, "check_ms": checkMS,
	}, nil
}

// checkPackage re-typechecks one package with edited bytes substituted for
// one file. Deps come from the snapshot's *types.Package graph.
func (s *Snapshot) checkPackage(p *packages.Package, edited string, content []byte) ([]Diagnostic, *Reject) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		var src any
		if name == edited {
			src = content
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, &Reject{Reason: "package does not parse", Detail: err.Error()}
		}
		if hasImportC(f) {
			// ponytail: cgo packages skip scoped checking; full reload on
			// next query still validates. Upgrade path: overlay packages.Load.
			return nil, nil
		}
		files = append(files, f)
	}
	var diags []Diagnostic
	conf := types.Config{
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if imp, ok := p.Imports[path]; ok && imp.Types != nil {
				return imp.Types, nil
			}
			return nil, fmt.Errorf("package %q not in snapshot", path)
		}),
		Sizes: types.SizesFor("gc", runtime.GOARCH),
		Error: func(err error) {
			if te, ok := err.(types.Error); ok {
				diags = append(diags, Diagnostic{Pos: te.Fset.Position(te.Pos).String(), Msg: te.Msg})
			} else {
				diags = append(diags, Diagnostic{Msg: err.Error()})
			}
		},
	}
	if p.Module != nil && p.Module.GoVersion != "" {
		conf.GoVersion = "go" + p.Module.GoVersion
	}
	conf.Check(p.PkgPath, fset, files, nil)
	return diags, nil
}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func hasImportC(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path.Value == `"C"` {
			return true
		}
	}
	return false
}

func findFuncDecl(p *packages.Package, fn *types.Func) (*ast.FuncDecl, string) {
	for _, file := range p.Syntax {
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Pos() == fn.Pos() {
				return fd, p.Fset.Position(file.Pos()).Filename
			}
		}
	}
	return nil, ""
}
