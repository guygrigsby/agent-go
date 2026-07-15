// Package snapshot holds a typechecked view of one Go workspace and answers
// semantic queries and validated mutations against it.
//
// Mutations never re-typecheck the world. The dirty set is the packages
// whose files changed plus, when an edit can alter a method set or exported
// API, the transitive reverse importers of the target package. Those are
// re-typechecked in dependency order against the in-memory graph and
// spliced into the snapshot, or rolled back wholesale on any diagnostic.
package snapshot

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
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
	// DidYouMean carries concrete repairs: candidate package paths or
	// symbols. Rejections are the agent's error channel; a bare "not
	// found" produces flailing retries, a candidate list produces the
	// correct next call.
	DidYouMean []string `json:"did_you_mean,omitempty"`
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
	loaded bool
	rev    map[string][]*packages.Package // package ID -> importers
	gens   map[string]int64               // package path -> generation
}

func New(dir string) *Snapshot {
	return &Snapshot{dir: dir}
}

// load typechecks the whole workspace from scratch. Caller holds mu.
// This runs once per daemon (and again only after external edits); all
// mutation revalidation goes through retypecheck instead.
func (s *Snapshot) load() (int64, error) {
	cfg := &packages.Config{Mode: loadMode, Dir: s.dir, Tests: true, Fset: token.NewFileSet()}
	start := time.Now()
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return 0, err
	}
	s.pkgs = pkgs
	s.fset = cfg.Fset
	s.rev = nil
	s.mtimes = map[string]time.Time{}
	for _, p := range pkgs {
		for _, f := range p.CompiledGoFiles {
			s.noteWrite(f)
		}
	}
	s.loaded = true
	s.bumpGenerations(s.workspacePackages())
	return time.Since(start).Milliseconds(), nil
}

func (s *Snapshot) noteWrite(file string) {
	if fi, err := os.Stat(file); err == nil {
		s.mtimes[file] = fi.ModTime()
	}
}

// ensureFresh reloads only when the snapshot has never loaded or a file
// changed behind the daemon's back. Returns reload cost in ms.
func (s *Snapshot) ensureFresh() (int64, error) {
	if s.loaded {
		fresh := true
		for f, t := range s.mtimes {
			fi, err := os.Stat(f)
			if err != nil || !fi.ModTime().Equal(t) {
				fresh = false
				break
			}
		}
		if fresh {
			return 0, nil
		}
	}
	return s.load()
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

// errorsIn reports diagnostics recorded on exactly these packages. Used to
// scope mutation preflight to the dirty set: rot elsewhere in the workspace
// must not block an unrelated edit.
func errorsIn(pkgs []*packages.Package) []Diagnostic {
	var diags []Diagnostic
	seen := map[string]bool{}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			if key := e.Pos + e.Msg; !seen[key] {
				seen[key] = true
				diags = append(diags, Diagnostic{Pos: e.Pos, Msg: e.Msg})
			}
		}
	}
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

// findObject resolves a symbol in the package's primary variant first, then
// in test variants — test helpers declared in _test.go files live only in
// the "pkg [pkg.test]" variant's scope.
func (s *Snapshot) findObject(pkgPath, sym string) (*packages.Package, types.Object, *Reject) {
	primary := s.primary(pkgPath)
	if primary == nil {
		return nil, nil, &Reject{Reason: "package not found", Detail: pkgPath,
			DidYouMean: s.suggestPackages(pkgPath)}
	}
	variants := []*packages.Package{primary}
	for _, p := range s.pkgs {
		if p.PkgPath == pkgPath && p.Types != nil && p != primary {
			variants = append(variants, p)
		}
	}
	recv, name, isSel := strings.Cut(sym, ".")
	for _, p := range variants {
		scope := p.Types.Scope()
		if !isSel {
			if obj := scope.Lookup(sym); obj != nil {
				return p, obj, nil
			}
			continue
		}
		recvObj := scope.Lookup(recv)
		if recvObj == nil {
			continue
		}
		if obj, _, _ := types.LookupFieldOrMethod(recvObj.Type(), true, p.Types, name); obj != nil {
			return p, obj, nil
		}
	}
	if isSel {
		if primary.Types.Scope().Lookup(recv) == nil {
			return nil, nil, &Reject{Reason: "receiver type not found", Detail: pkgPath + "." + recv,
				DidYouMean: s.suggestSymbols(pkgPath, recv)}
		}
		return nil, nil, &Reject{Reason: "method or field not found", Detail: pkgPath + "." + sym,
			DidYouMean: s.suggestSymbols(pkgPath, name)}
	}
	return nil, nil, &Reject{Reason: "symbol not found", Detail: pkgPath + "." + sym,
		DidYouMean: s.suggestSymbols(pkgPath, sym)}
}

// suggestPackages finds loaded package paths close to a miss: exact after
// dot/slash normalization, then suffix or substring matches.
func (s *Snapshot) suggestPackages(miss string) []string {
	norm := strings.ReplaceAll(miss, ".", "/")
	base := miss[strings.LastIndexAny(miss, "./")+1:]
	var exact, close_ []string
	seen := map[string]bool{}
	for _, p := range s.pkgs {
		if p.Types == nil || seen[p.PkgPath] {
			continue
		}
		seen[p.PkgPath] = true
		if strings.ReplaceAll(p.PkgPath, ".", "/") == norm {
			exact = append(exact, p.PkgPath)
		} else if strings.HasSuffix(p.PkgPath, "/"+base) || p.PkgPath == base {
			close_ = append(close_, p.PkgPath)
		}
	}
	out := append(exact, close_...)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// suggestSymbols finds near-miss symbols in a package: case-insensitive
// equality first, then substring either way.
func (s *Snapshot) suggestSymbols(pkgPath, name string) []string {
	p := s.primary(pkgPath)
	if p == nil {
		return nil
	}
	lower := strings.ToLower(name)
	var hits []string
	scope := p.Types.Scope()
	for _, n := range scope.Names() {
		ln := strings.ToLower(n)
		if ln == lower || strings.Contains(ln, lower) || strings.Contains(lower, ln) {
			hits = append(hits, n)
		}
		if tn, ok := scope.Lookup(n).(*types.TypeName); ok {
			for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
				m := sel.Obj().Name()
				if lm := strings.ToLower(m); lm == lower || strings.Contains(lm, lower) {
					hits = append(hits, n+"."+m)
				}
			}
		}
		if len(hits) >= 6 {
			break
		}
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
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

// objKey is a splice-stable identity. Package-scope objects (and their
// fields and methods) get pkgpath#objectpath, which survives re-typechecking
// and matches across test-variant packages and stale importers. Locals fall
// back to defining position; they are only ever referenced from their own
// freshly-checked package.
func (s *Snapshot) objKey(o types.Object) string {
	if o.Pkg() != nil {
		if path, err := objectpath.For(o); err == nil {
			return o.Pkg().Path() + "#" + string(path)
		}
	}
	return "pos:" + s.fset.Position(o.Pos()).String()
}

// refInfo is one reference to an object.
type refInfo struct {
	pos token.Position
	pkg string
	def bool
}

// references returns every Def and Use of obj across the workspace,
// deduplicated across test-variant packages.
func (s *Snapshot) references(obj types.Object) []refInfo {
	key := s.objKey(obj)
	seen := map[string]bool{}
	var refs []refInfo
	for _, p := range s.pkgs {
		// Test-main packages hold generated registration code in the build
		// cache; their references are synthetic and regenerate on the next
		// go test.
		if p.TypesInfo == nil || strings.HasSuffix(p.ID, ".test") {
			continue
		}
		add := func(idPos token.Pos, o types.Object, def bool) {
			// Name check first: objKey is too expensive for every ident.
			if o == nil || o.Name() != obj.Name() || s.objKey(o) != key {
				return
			}
			pos := p.Fset.Position(idPos)
			if !seen[pos.String()] {
				seen[pos.String()] = true
				refs = append(refs, refInfo{pos, p.PkgPath, def})
			}
		}
		for id, o := range p.TypesInfo.Defs {
			add(id.Pos(), o, true)
		}
		for id, o := range p.TypesInfo.Uses {
			add(id.Pos(), o, false)
		}
	}
	return refs
}

// workspacePackages enumerates every main-module package in the full load
// graph: roots, test variants, and test-forked dependency copies. Forks
// (e.g. "controller [credential/vault.test]") are not roots but are real
// compilation units holding their own types; ignoring them leaves stale
// type identities in the graph after a splice.
func (s *Snapshot) workspacePackages() []*packages.Package {
	var all []*packages.Package
	packages.Visit(s.pkgs, nil, func(p *packages.Package) {
		if p.Module != nil && p.Module.Main {
			all = append(all, p)
		}
	})
	return all
}

// reverse builds the importer graph over every workspace package variant,
// forks included.
func (s *Snapshot) reverse() map[string][]*packages.Package {
	if s.rev == nil {
		s.rev = map[string][]*packages.Package{}
		for _, p := range s.workspacePackages() {
			for _, imp := range p.Imports {
				s.rev[imp.ID] = append(s.rev[imp.ID], p)
			}
		}
	}
	return s.rev
}

// affected returns every variant of pkgPath plus its transitive reverse
// importers: the packages whose typechecking could observe a method-set or
// API change in pkgPath.
func (s *Snapshot) affected(pkgPath string) []*packages.Package {
	rev := s.reverse()
	var queue []*packages.Package
	for _, p := range s.workspacePackages() {
		if p.PkgPath == pkgPath {
			queue = append(queue, p)
		}
	}
	seen := map[string]bool{}
	var out []*packages.Package
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		out = append(out, p)
		queue = append(queue, rev[p.ID]...)
	}
	return out
}

// dirtyByFiles returns every loaded package variant that compiles any of
// the given files.
func (s *Snapshot) dirtyByFiles(files map[string]bool) []*packages.Package {
	var out []*packages.Package
	for _, p := range s.workspacePackages() {
		for _, f := range p.CompiledGoFiles {
			if files[f] {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

type pkgState struct {
	types  *types.Package
	info   *types.Info
	syntax []*ast.File
	errs   []packages.Error
}

// retypecheck re-typechecks the dirty packages in dependency order, reading
// current file contents from disk, and splices results into the snapshot in
// place. Importers see spliced results immediately because Imports shares
// package pointers. Any diagnostic rolls back every splice.
//
// ponytail: each splice re-parses into the shared FileSet, which only grows;
// a long-lived daemon on a huge repo will accumulate. Idle-exit bounds it.
func (s *Snapshot) retypecheck(dirty []*packages.Package) ([]Diagnostic, int, error) {
	var work []*packages.Package
	seen := map[string]bool{}
	for _, p := range dirty {
		// Test-main packages have synthesized sources and no user symbols.
		if p == nil || p.Types == nil || seen[p.ID] || strings.HasSuffix(p.ID, ".test") {
			continue
		}
		seen[p.ID] = true
		work = append(work, p)
	}
	order := topo(work)
	if os.Getenv("AGO_DEBUG_DIRTY") != "" {
		fmt.Fprintf(os.Stderr, "retypecheck: %d dirty, order:\n", len(order))
		for i, p := range order {
			fmt.Fprintf(os.Stderr, "  %3d %s\n", i, p.ID)
		}
		if len(order) != len(work) {
			fmt.Fprintf(os.Stderr, "  TOPO DROPPED %d PACKAGES\n", len(work)-len(order))
		}
	}

	saved := map[*packages.Package]pkgState{}
	restore := func() {
		for p, st := range saved {
			p.Types, p.TypesInfo, p.Syntax, p.Errors = st.types, st.info, st.syntax, st.errs
		}
	}
	var diags []Diagnostic
	for _, p := range order {
		files, cgo, parseDiags := s.parsePkg(p)
		if cgo {
			// cgo needs the go tool's preprocessing; splice the whole world.
			restore()
			ms, err := s.load()
			if err != nil {
				return nil, 0, err
			}
			_ = ms
			return s.errors(), len(order), nil
		}
		if len(parseDiags) > 0 {
			restore()
			return parseDiags, len(order), nil
		}
		info := &types.Info{
			Defs:         map[*ast.Ident]types.Object{},
			Uses:         map[*ast.Ident]types.Object{},
			Types:        map[ast.Expr]types.TypeAndValue{},
			Selections:   map[*ast.SelectorExpr]*types.Selection{},
			Implicits:    map[ast.Node]types.Object{},
			Scopes:       map[ast.Node]*types.Scope{},
			Instances:    map[*ast.Ident]types.Instance{},
			FileVersions: map[*ast.File]string{},
		}
		var perr []Diagnostic
		conf := types.Config{
			Importer: importerFor(p),
			Sizes:    types.SizesFor("gc", runtime.GOARCH),
			Error: func(err error) {
				if te, ok := err.(types.Error); ok {
					perr = append(perr, Diagnostic{Pos: te.Fset.Position(te.Pos).String(), Msg: te.Msg})
				} else {
					perr = append(perr, Diagnostic{Msg: err.Error()})
				}
			},
		}
		if p.Module != nil && p.Module.GoVersion != "" {
			conf.GoVersion = "go" + p.Module.GoVersion
		}
		tpkg, _ := conf.Check(p.PkgPath, s.fset, files, info)
		saved[p] = pkgState{p.Types, p.TypesInfo, p.Syntax, p.Errors}
		p.Types, p.TypesInfo, p.Syntax, p.Errors = tpkg, info, files, nil
		diags = append(diags, perr...)
	}
	if len(diags) > 0 {
		restore()
	} else {
		s.bumpGenerations(order)
	}
	return diags, len(order), nil
}

// parsePkg parses a package's current files from disk into the shared FileSet.
func (s *Snapshot) parsePkg(p *packages.Package) ([]*ast.File, bool, []Diagnostic) {
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		f, err := parser.ParseFile(s.fset, name, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, false, []Diagnostic{{Pos: name, Msg: err.Error()}}
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"C"` {
				return nil, true, nil
			}
		}
		files = append(files, f)
	}
	return files, false, nil
}

func importerFor(p *packages.Package) types.Importer {
	return importerFunc(func(path string) (*types.Package, error) {
		if imp, ok := p.Imports[path]; ok && imp.Types != nil {
			return imp.Types, nil
		}
		// p.Imports only knows the dependency graph packages.Load walked
		// before this edit. A patch can splice in a fresh import goimports
		// added that nothing in p previously used (wrap_error's
		// "fmt.Errorf(...)" being the case that surfaced this: a file with no
		// prior fmt import gets one added by imports.Process at end-of-list
		// formatting). Standard-library paths resolve independent of this
		// workspace's module graph, so fall back to the running toolchain's
		// own importer for those. A third-party or module-local package
		// introduced this way is not covered — that needs a full workspace
		// reload to discover via the real module graph (as UpsertDecl already
		// does when it creates a new package), out of scope for this
		// fallback.
		if pkg, err := importer.Default().Import(path); err == nil {
			return pkg, nil
		}
		return nil, fmt.Errorf("package %q not in snapshot", path)
	})
}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// topo orders packages so dependencies precede importers (edges within the
// set only).
func topo(work []*packages.Package) []*packages.Package {
	inSet := map[string]*packages.Package{}
	for _, p := range work {
		inSet[p.ID] = p
	}
	indeg := map[string]int{}
	for _, p := range work {
		for _, imp := range p.Imports {
			if _, ok := inSet[imp.ID]; ok {
				indeg[p.ID]++
			}
		}
	}
	var order []*packages.Package
	var ready []*packages.Package
	for _, p := range work {
		if indeg[p.ID] == 0 {
			ready = append(ready, p)
		}
	}
	for len(ready) > 0 {
		p := ready[0]
		ready = ready[1:]
		order = append(order, p)
		for _, q := range work {
			for _, imp := range q.Imports {
				if imp.ID == p.ID {
					if indeg[q.ID]--; indeg[q.ID] == 0 {
						ready = append(ready, q)
					}
				}
			}
		}
	}
	return order
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

// Search finds workspace symbols by case-insensitive substring: package
// scope names, struct fields, and methods. This is the discovery op — the
// bridge from a natural-language name fragment to an exact symbol address.
func (s *Snapshot) Search(query string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, &Reject{Reason: "empty query"}
	}
	q := strings.ToLower(query)
	type hit struct {
		Pkg  string `json:"pkg"`
		Sym  string `json:"sym"`
		Kind string `json:"kind"`
		Pos  string `json:"pos"`
	}
	const limit = 200
	var hits []hit
	seen := map[string]bool{}
	add := func(pkg, sym string, obj types.Object) {
		key := pkg + "." + sym
		if seen[key] || len(hits) >= limit {
			return
		}
		seen[key] = true
		hits = append(hits, hit{pkg, sym, objKind(obj), s.fset.Position(obj.Pos()).String()})
	}
	for _, p := range s.pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if strings.Contains(strings.ToLower(name), q) {
				add(p.PkgPath, name, obj)
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			if st, ok := tn.Type().Underlying().(*types.Struct); ok {
				for fld := range st.Fields() {
					if strings.Contains(strings.ToLower(fld.Name()), q) {
						add(p.PkgPath, name+"."+fld.Name(), fld)
					}
				}
			}
			for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
				if fn := sel.Obj(); strings.Contains(strings.ToLower(fn.Name()), q) {
					add(p.PkgPath, name+"."+fn.Name(), fn)
				}
			}
		}
	}
	return map[string]any{
		"status": "ok", "query": query, "count": len(hits),
		"truncated": len(hits) >= limit, "symbols": hits, "load_ms": ms,
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
	var refs []ref
	for _, r := range s.references(obj) {
		refs = append(refs, ref{Pos: r.pos.String(), Pkg: r.pkg, Def: r.def})
	}
	return map[string]any{
		"status": "ok", "symbol": pkgPath + "." + sym,
		"count": len(refs), "refs": refs, "load_ms": ms,
	}, nil
}

// SetBody replaces a function's body. A body edit cannot change the
// package's exported API, so the dirty set is just the packages compiling
// the edited file (the package and its internal-test variant).
func (s *Snapshot) SetBody(pkgPath, sym, body string) (map[string]any, error) {
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
	decl, filename := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	if diags := errorsIn(s.dirtyByFiles(map[string]bool{filename: true})); len(diags) > 0 {
		return nil, &Reject{Reason: "affected packages have pre-existing errors", Diagnostics: diags}
	}
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	lbrace := s.fset.Position(decl.Body.Lbrace).Offset
	rbrace := s.fset.Position(decl.Body.Rbrace).Offset
	formatted, ferr := spliceBody(src, lbrace, rbrace, body)
	if ferr != nil {
		return nil, &Reject{Reason: "new body does not parse", Detail: ferr.Error()}
	}

	if err := os.WriteFile(filename, formatted, 0o644); err != nil {
		return nil, err
	}
	start := time.Now()
	diags, n, err := s.retypecheck(s.dirtyByFiles(map[string]bool{filename: true}))
	checkMS := time.Since(start).Milliseconds()
	if err != nil {
		s.rollback(map[string][]byte{filename: src})
		return nil, err
	}
	if len(diags) > 0 {
		s.rollback(map[string][]byte{filename: src})
		return nil, &Reject{Reason: "edit does not typecheck", Diagnostics: diags}
	}
	s.noteWrite(filename)
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "file": filename,
		"load_ms": ms, "check_ms": checkMS, "packages_rechecked": n,
		"generation": s.generation(pkgPath, sym),
	}, nil
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
