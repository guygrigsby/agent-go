// Package snapshot holds a typechecked view of one Go workspace and answers
// semantic queries and validated mutations against it.
//
// Mutations never re-typecheck the world. The dirty set is the packages
// whose files changed plus, when an edit can alter a method set or exported
// API, the transitive reverse importers of the target package. Those are
// re-typechecked in dependency order against the in-memory graph and
// spliced into the snapshot, or rolled back wholesale on any NEW diagnostic.
// Diagnostics the dirty set already carried before the edit (real-world
// repos accumulate unrelated rot) are captured as a baseline at preflight
// and filtered out afterward: the contract is "no new errors", not "no
// errors".
package snapshot

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"runtime"
	"sort"
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
	// PossibleRepairs carry the correct next call whole: the agent
	// resends Call verbatim instead of composing a fix from prose.
	PossibleRepairs []Repair `json:"possible_repairs,omitempty"`
}

// Repair is one complete, paste-ready next call.
type Repair struct {
	Why  string         `json:"why,omitempty"`
	Call map[string]any `json:"call"`
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

	// importCache and stdImp back retypecheck's import fallback for paths
	// the dirty package's own pre-edit graph does not carry. One shared
	// cache per snapshot: two dirty packages gaining the same new import
	// must resolve to one *types.Package, or cross-package uses of its
	// types mismatch ("context.Context does not implement context.Context").
	importCache map[string]*types.Package
	impMu       sync.Mutex // importCache/stdImp: workers race during parallel retypecheck
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
	s.importCache = nil
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
			if e.Pos == "" && strings.HasPrefix(e.Msg, "# ") {
				continue // go list compile-header noise, not a diagnostic
			}
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
			if e.Pos == "" && strings.HasPrefix(e.Msg, "# ") {
				continue // go list compile-header noise, not a diagnostic
			}
			if key := e.Pos + e.Msg; !seen[key] {
				seen[key] = true
				diags = append(diags, Diagnostic{Pos: e.Pos, Msg: e.Msg})
			}
		}
	}
	return diags
}

// errorSet keys diagnostics as pos|msg — the identity a diagnostic keeps
// across an incremental retypecheck when the code around it is untouched —
// for baseline capture. A nil set means no pre-existing diagnostics.
func errorSet(diags []Diagnostic) map[string]bool {
	if len(diags) == 0 {
		return nil
	}
	set := make(map[string]bool, len(diags))
	for _, d := range diags {
		set[d.Pos+"|"+d.Msg] = true
	}
	return set
}

// filterNew drops diagnostics already present in baseline, leaving only the
// ones the current edit introduced. Position-keyed, so a pre-existing
// diagnostic whose position the edit itself shifts reads as new — the safe
// direction: at worst a genuinely-unrelated error rejects an edit, never
// the reverse.
func filterNew(diags []Diagnostic, baseline map[string]bool) []Diagnostic {
	if len(baseline) == 0 || len(diags) == 0 {
		return diags
	}
	var out []Diagnostic
	for _, d := range diags {
		if !baseline[d.Pos+"|"+d.Msg] {
			out = append(out, d)
		}
	}
	return out
}

// addPreExisting records the count of tolerated pre-existing diagnostics on
// an accepted response, so baseline rot stays visible to the caller.
func addPreExisting(res map[string]any, baseline map[string]bool) map[string]any {
	if len(baseline) > 0 {
		res["pre_existing"] = len(baseline)
	}
	return res
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

// suggestSymbols finds near-miss symbols in a package, in three tiers:
// case-insensitive equality, then substring either way, then subsequence
// either way (all chars in order — a stale name from before a rename like
// extractLabels -> extractSwarmLabels is a subsequence of the new name, not
// a substring). Subsequence needs len(query) >= 4 or two-letter fragments
// match everything. Scans every loaded variant of the package (primary plus
// test variants), not just the primary — a TestXxx symbol declared only in
// a _test.go file lives in the test variant's scope alone, and a rejection
// naming it (add_test_case et al. addressing an unknown test) must be able
// to suggest it back.
func (s *Snapshot) suggestSymbols(pkgPath, name string) []string {
	p := s.primary(pkgPath)
	if p == nil {
		return nil
	}
	variants := []*packages.Package{p}
	for _, v := range s.pkgs {
		if v.PkgPath == pkgPath && v.Types != nil && v != p {
			variants = append(variants, v)
		}
	}
	lower := strings.ToLower(name)
	tiers := make([][]string, 3)
	seen := map[string]bool{}
	add := func(n, cand string) {
		if seen[n] {
			return
		}
		lc := strings.ToLower(cand)
		tier := -1
		switch {
		case lc == lower:
			tier = 0
		case strings.Contains(lc, lower) || strings.Contains(lower, lc):
			tier = 1
		case len(lower) >= 4 && (isSubsequence(lower, lc) || isSubsequence(lc, lower)):
			tier = 2
		default:
			return
		}
		seen[n] = true
		tiers[tier] = append(tiers[tier], n)
	}
	for _, p := range variants {
		scope := p.Types.Scope()
		for _, n := range scope.Names() {
			add(n, n)
			if tn, ok := scope.Lookup(n).(*types.TypeName); ok {
				for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
					m := sel.Obj().Name()
					add(n+"."+m, m)
				}
			}
		}
	}
	var hits []string
	for _, t := range tiers {
		hits = append(hits, t...)
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
}

// isSubsequence reports whether every rune of needle appears in hay in
// order (not necessarily contiguously). Both sides pre-lowered by caller.
func isSubsequence(needle, hay string) bool {
	rs := []rune(needle)
	i := 0
	for _, r := range hay {
		if i < len(rs) && r == rs[i] {
			i++
		}
	}
	return i == len(rs)
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
// deduplicated across test-variant packages, in position order. The idents
// come from TypesInfo's Defs/Uses maps, whose iteration order is random;
// sorting here keeps every consumer (Refs output, rename and delete-decl
// edit application) deterministic.
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
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i].pos, refs[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
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
// package pointers. Any diagnostic NOT in baseline (pos|msg keys captured
// from the dirty set at the caller's preflight, before any bytes changed)
// rolls back every splice; only new diagnostics are returned. Pre-existing
// diagnostics are tolerated: the splice lands and each package keeps them
// on p.Errors, so the next mutation's preflight still sees the rot.
//
// Parse failures are the exception: a dirty package that no longer parses
// cannot be spliced at all, so those reject unfiltered even when the parse
// error predates the edit.
//
// ponytail: each splice re-parses into the shared FileSet, which only grows;
// a long-lived daemon on a huge repo will accumulate. Idle-exit bounds it.
func (s *Snapshot) retypecheck(dirty []*packages.Package, baseline map[string]bool) ([]Diagnostic, int, error) {
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

	workers := runtime.GOMAXPROCS(0)
	if os.Getenv("AGO_SERIAL_TYPECHECK") != "" {
		workers = 1
	}

	// Phase 1: parse every dirty package concurrently. Parsing needs no
	// dependency order, and the parsed files are what the scheduling edges
	// must come from: an edit can CHANGE the import graph (move_decl adds
	// an import the pre-edit p.Imports doesn't know), so scheduling off
	// the stale graph would let an importer check against unspliced types.
	filesBy := make([][]*ast.File, len(order))
	parseFail := make([][]Diagnostic, len(order))
	sawCgo := false
	var pmu sync.Mutex
	var pwg sync.WaitGroup
	psem := make(chan struct{}, workers)
	for i := range order {
		pwg.Add(1)
		go func(i int) {
			defer pwg.Done()
			psem <- struct{}{}
			defer func() { <-psem }()
			files, cgo, pd := s.parsePkg(order[i])
			pmu.Lock()
			if cgo {
				sawCgo = true
			} else if len(pd) > 0 {
				parseFail[i] = pd
			} else {
				filesBy[i] = files
			}
			pmu.Unlock()
		}(i)
	}
	pwg.Wait()
	if sawCgo {
		// cgo needs the go tool's preprocessing; splice the whole world.
		if _, err := s.load(); err != nil {
			return nil, 0, err
		}
		return filterNew(s.errors(), baseline), len(order), nil
	}
	for _, pd := range parseFail {
		// First parse failure in topo order, matching the old contract.
		if pd != nil {
			return pd, len(order), nil
		}
	}

	// The import-fallback cache may hold a pointer to a package this round
	// re-splices; a cached stale *types.Package would leak old types into
	// fresh importers.
	s.impMu.Lock()
	for _, p := range order {
		delete(s.importCache, p.PkgPath)
	}
	s.impMu.Unlock()

	// Phase 2: dependency-counter scheduling over POST-EDIT edges — each
	// parsed file's import specs, resolved to the in-set primary variant.
	// A dependent is released only after its dependency's results are
	// spliced, so every importer observes finished packages exactly as the
	// serial loop guaranteed. Results are kept per topo index and
	// flattened in topo order, so diagnostics stay byte-identical to the
	// serial driver (AGO_SERIAL_TYPECHECK=1 forces one worker for A/B).
	primaryIdx := map[string]int{}
	idxByID := map[string]int{}
	for i, p := range order {
		idxByID[p.ID] = i
		if p.ID == p.PkgPath {
			primaryIdx[p.PkgPath] = i
		}
	}
	depCount := make([]int, len(order))
	dependents := make([][]int, len(order))
	for i := range order {
		seenDep := map[int]bool{}
		for _, f := range filesBy[i] {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				// The graph knows which VARIANT this import resolves to —
				// a test-variant importer may depend on a test-variant dep
				// ("config [foo.test]"), and binding to the primary would
				// let this package check before its actual import target
				// spliced. The primary map only catches imports the edit
				// itself introduced, which the pre-edit graph cannot know.
				j, ok := -1, false
				if node := order[i].Imports[path]; node != nil {
					j, ok = idxByID[node.ID]
				} else {
					j, ok = primaryIdx[path]
				}
				if !ok || j == i || seenDep[j] {
					continue
				}
				seenDep[j] = true
				depCount[i]++
				dependents[j] = append(dependents[j], i)
			}
		}
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, workers)
		diagsBy = make([][]Diagnostic, len(order))
		done    = make([]bool, len(order))
	)
	splice := func(i int, tpkg *types.Package, info *types.Info, perr []Diagnostic) {
		// Caller holds mu.
		p := order[i]
		saved[p] = pkgState{p.Types, p.TypesInfo, p.Syntax, p.Errors}
		// Keep the fresh diagnostics on p.Errors (nil when clean): when a
		// splice with tolerated pre-existing rot lands, the next mutation's
		// preflight must still observe that rot to baseline it.
		var perrs []packages.Error
		for _, d := range perr {
			perrs = append(perrs, packages.Error{Pos: d.Pos, Msg: d.Msg, Kind: packages.TypeError})
		}
		p.Types, p.TypesInfo, p.Syntax, p.Errors = tpkg, info, filesBy[i], perrs
		diagsBy[i] = perr
		done[i] = true
	}
	var launch func(i int)
	launch = func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tpkg, info, perr := s.checkOne(order[i], filesBy[i])
			mu.Lock()
			splice(i, tpkg, info, perr)
			for _, d := range dependents[i] {
				if depCount[d]--; depCount[d] == 0 {
					launch(d)
				}
			}
			mu.Unlock()
		}()
	}
	mu.Lock()
	for i := range order {
		if depCount[i] == 0 {
			launch(i)
		}
	}
	mu.Unlock()
	wg.Wait()
	// A post-edit import cycle leaves nodes unscheduled (their counters
	// never reach zero). Check them serially in topo order; the checker
	// itself reports the cycle as a diagnostic.
	for i := range order {
		if !done[i] {
			tpkg, info, perr := s.checkOne(order[i], filesBy[i])
			splice(i, tpkg, info, perr)
		}
	}

	var diags []Diagnostic
	for _, d := range diagsBy {
		diags = append(diags, d...)
	}
	if fresh := filterNew(diags, baseline); len(fresh) > 0 {
		restore()
		return fresh, len(order), nil
	}
	s.bumpGenerations(order)
	return nil, len(order), nil
}

// checkOne typechecks one package's freshly parsed files. Safe to run
// concurrently across packages: the shared FileSet is synchronized, an
// imported *types.Package is immutable once its check completed (the
// scheduler releases dependents only after the splice), and the import
// fallback cache carries its own lock.
func (s *Snapshot) checkOne(p *packages.Package, files []*ast.File) (*types.Package, *types.Info, []Diagnostic) {
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
		Importer: importerFor(s, p),
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
	return tpkg, info, perr
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

func importerFor(s *Snapshot, p *packages.Package) types.Importer {
	return importerFunc(func(path string) (*types.Package, error) {
		if imp, ok := p.Imports[path]; ok && imp.Types != nil {
			return imp.Types, nil
		}
		return s.importFallback(path)
	})
}

// importFallback resolves an import path the dirty package's own pre-edit
// graph does not carry — a fresh import goimports added that nothing in p
// previously used (wrap_error's "fmt.Errorf(...)", add_param's parameter
// type). Resolution order matters for type identity: first anywhere else
// in the loaded workspace graph, so uses agree with packages already
// typechecked against it; then, for paths new to the whole workspace, one
// snapshot-shared toolchain importer, so two dirty packages gaining the
// same new import see one *types.Package. A third-party or module-local
// package introduced this way is not covered — that needs a full workspace
// reload to discover via the real module graph (as UpsertDecl already does
// when it creates a new package). Caller holds mu.
func (s *Snapshot) importFallback(path string) (*types.Package, error) {
	s.impMu.Lock()
	defer s.impMu.Unlock()
	if pkg, ok := s.importCache[path]; ok {
		return pkg, nil
	}
	// Identity matters: the returned package must be the same object the
	// loader gave every workspace root, or cross-package uses mismatch.
	// Dependencies are not source-loaded (no NeedDeps); a root's DIRECT
	// imports were read from their own export files and are complete, but
	// anything deeper is a shallow stub holding only the names the export
	// data happened to reference — unusable as an importer result.
	var found *types.Package
	for _, p := range s.pkgs {
		if p.Types == nil {
			continue
		}
		if p.PkgPath == path {
			found = p.Types
			break
		}
		for _, imp := range p.Types.Imports() {
			if imp.Path() == path {
				found = imp
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		// A path in no root's closure is genuinely new to the snapshot, so
		// a fresh on-demand load cannot mismatch any existing usage.
		// ponytail: two new imports loaded separately in one round get
		// separate universes for their shared internals; a third package
		// mixing their types would mismatch — reload-the-world territory.
		cfg := &packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps, Dir: s.dir}
		pkgs, err := packages.Load(cfg, path)
		if err != nil || len(pkgs) == 0 || pkgs[0].Types == nil {
			return nil, fmt.Errorf("package %q not in snapshot", path)
		}
		found = pkgs[0].Types
	}
	if s.importCache == nil {
		s.importCache = map[string]*types.Package{}
	}
	s.importCache[path] = found
	return found, nil
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
// Hits arrive in package load order, scope names sorted within each
// package — deterministic, so offset pages are stable.
func (s *Snapshot) Search(query string, offset int) (map[string]any, error) {
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
	var hits []hit
	seen := map[string]bool{}
	add := func(pkg, sym string, obj types.Object) {
		key := pkg + "." + sym
		if seen[key] {
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
	return page(map[string]any{
		"status": "ok", "query": query, "load_ms": ms,
	}, "symbols", hits, offset), nil
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
	res := map[string]any{
		"status": "ok", "name": obj.Name(), "kind": objKind(obj),
		"type":     types.TypeString(obj.Type(), types.RelativeTo(p.Types)),
		"exported": obj.Exported(), "pos": s.fset.Position(obj.Pos()).String(),
		"pkg": pkgPath, "load_ms": ms,
	}
	// A type's inspect is the discovery move for its methods: without this
	// list an agent hunting a method name has nothing to navigate by
	// (bench evidence: vault_cfff8d42, 40 method-not-found rejects).
	if tn, ok := obj.(*types.TypeName); ok {
		var methods []map[string]any
		ms := types.NewMethodSet(types.NewPointer(tn.Type()))
		for sel := range ms.Methods() {
			f := sel.Obj()
			methods = append(methods, map[string]any{
				"name":      f.Name(),
				"signature": types.TypeString(f.Type(), types.RelativeTo(p.Types)),
			})
		}
		sort.Slice(methods, func(i, j int) bool {
			return methods[i]["name"].(string) < methods[j]["name"].(string)
		})
		if len(methods) > 0 {
			res["methods"] = methods
		}
	}
	return res, nil
}

// Refs lists every reference to a symbol, position-sorted (references
// sorts at source), paged by offset.
func (s *Snapshot) Refs(pkgPath, sym string, offset int) (map[string]any, error) {
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
	return page(map[string]any{
		"status": "ok", "symbol": pkgPath + "." + sym, "load_ms": ms,
	}, "refs", refs, offset), nil
}

// setBodyEdit locates sym's function body, returning the byte offsets of
// its opening and closing braces plus the baseline of pre-existing
// diagnostics in the file's dirty set (captured here, where the dirty set
// is already in hand; the caller filters its post-splice retypecheck
// against it). Pure position computation otherwise: callers read the file
// themselves and splice body into place (SetBody via spliceBody, which also
// gofmts the whole file; the composable set_body op via the decl-op ledger,
// deferring formatting to patchComposable's end-of-list imports.Process).
func setBodyEdit(s *Snapshot, pkgPath, sym string) (filename string, lbrace, rbrace int, baseline map[string]bool, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, sym)
	if rej0 != nil {
		return "", 0, 0, nil, rej0
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return "", 0, 0, nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, filename := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return "", 0, 0, nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	baseline = errorSet(errorsIn(s.dirtyByFiles(map[string]bool{filename: true})))
	return filename, s.fset.Position(decl.Body.Lbrace).Offset, s.fset.Position(decl.Body.Rbrace).Offset, baseline, nil
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
	filename, lbrace, rbrace, baseline, rej := setBodyEdit(s, pkgPath, sym)
	if rej != nil {
		s.sugarRepairs(rej, "set_body",
			map[string]any{"pkg": pkgPath, "sym": sym, "body": body}, s.resolvesToFunc)
		return nil, rej
	}
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	formatted, ferr := spliceBody(src, lbrace, rbrace, body)
	if ferr != nil {
		return nil, &Reject{Reason: "new body does not parse", Detail: ferr.Error()}
	}

	if err := os.WriteFile(filename, formatted, 0o644); err != nil {
		return nil, err
	}
	start := time.Now()
	diags, n, err := s.retypecheck(s.dirtyByFiles(map[string]bool{filename: true}), baseline)
	checkMS := time.Since(start).Milliseconds()
	if err != nil {
		s.rollback(map[string][]byte{filename: src})
		return nil, err
	}
	if len(diags) > 0 {
		s.rollback(map[string][]byte{filename: src})
		return nil, diagnosticRepairs(&Reject{Reason: "edit does not typecheck", Diagnostics: diags})
	}
	s.noteWrite(filename)
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "file": filename,
		"load_ms": ms, "check_ms": checkMS, "packages_rechecked": n,
		"generation": s.generation(pkgPath, sym),
	}, baseline)
	s.attachView(res, pkgPath, sym)
	return res, nil
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
