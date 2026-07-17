package snapshot

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
)

// upsertDeclEdit locates where text's declaration belongs in an
// already-loaded pkgPath: an existing standalone declaration's range to
// replace (reusing findDeclRange), or the end of an already-existing
// agent.go to append to. needsCreate signals the one case the composable
// upsert_decl op cannot support: no agent.go exists yet, so landing the
// declaration means creating a brand-new file. A new file changes the
// package's CompiledGoFiles, which the incremental retypecheck path never
// picks up — only a full workspace reload does (see UpsertDecl's own
// s.loaded = false below) — and the composable pipeline has no such
// fallback mid-patch. The direct UpsertDecl method still handles it, same
// as it always has; needsCreate is a documented v1 ceiling for the
// composable op only.
func upsertDeclEdit(s *Snapshot, pkgPath, name, sym string, testDecl bool) (file string, start, end int, action string, needsCreate bool, rej *Reject) {
	p := s.primary(pkgPath)
	if p == nil {
		return "", 0, 0, "", false, &Reject{Reason: "package not found", Detail: pkgPath,
			DidYouMean: s.suggestPackages(pkgPath)}
	}
	// Primary first so non-test decls resolve to their primary file, then
	// every other variant: a decl living in a _test.go file is only in the
	// test variant's syntax, and missing it would append a duplicate.
	if file, start, end := s.findDeclRange(p, name, sym); file != "" {
		return file, start, end, "replaced", false, nil
	}
	for _, v := range s.pkgs {
		if v == p || v.PkgPath != pkgPath || v.Types == nil || strings.HasSuffix(v.ID, ".test") {
			continue
		}
		if file, start, end := s.findDeclRange(v, name, sym); file != "" {
			return file, start, end, "replaced", false, nil
		}
	}
	// A new test func lands in a _test.go file, mirroring move_decl's
	// landing rule: a Test func in a non-test file compiles but never runs.
	base := "agent.go"
	if testDecl {
		base = "agent_test.go"
	}
	agentFile := filepath.Join(filepath.Dir(p.Fset.Position(p.Syntax[0].Pos()).Filename), base)
	b, err := os.ReadFile(agentFile)
	if err != nil {
		return agentFile, 0, 0, "added", true, nil
	}
	return agentFile, len(b), len(b), "added", false, nil
}

// testFuncDecl reports whether text declares a func the go test runner
// owns (Test/Benchmark/Example/Fuzz), which must live in a _test.go file
// to actually execute.
func testFuncDecl(text string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "decl.go", "package _p\n\n"+text, parser.SkipObjectResolution)
	if err != nil || len(f.Decls) != 1 {
		return false
	}
	d, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || d.Recv != nil {
		return false
	}
	for _, prefix := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(d.Name.Name, prefix) {
			return true
		}
	}
	return false
}

// UpsertDecl adds or replaces one top-level declaration from source text.
// This is the authoring op: everything else reshapes existing code. The
// edited file goes through goimports before validation, so declarations may
// freely reference importable packages. New declarations land in agent.go
// (created on demand); a package path that does not exist yet is created
// under the module.
//
// ponytail: replacing a spec inside a grouped const/var/type block is
// rejected; split the group or extend the op when it matters.
func (s *Snapshot) UpsertDecl(pkgPath, text string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.ensureFresh()
	if err != nil {
		return nil, err
	}
	name, sym, rej := parseDeclText(text)
	if rej != nil {
		return nil, rej
	}

	p := s.primary(pkgPath)
	if p == nil {
		return s.upsertNewPackage(pkgPath, text, sym, ms)
	}

	file, start, end, action, needsCreate, rej := upsertDeclEdit(s, pkgPath, name, sym, testFuncDecl(text))
	if rej != nil {
		return nil, rej
	}
	var src []byte
	if needsCreate {
		src = []byte("package " + p.Types.Name() + "\n")
		start, end = len(src), len(src)
	} else {
		src, err = os.ReadFile(file)
		if err != nil {
			return nil, err
		}
	}

	var buf strings.Builder
	buf.Write(src[:start])
	if start > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString(strings.TrimSpace(text))
	buf.WriteString("\n")
	buf.Write(src[end:])
	fixed, err := imports.Process(file, []byte(buf.String()), nil)
	if err != nil {
		return nil, &Reject{Reason: "declaration does not parse in place", Detail: err.Error()}
	}

	preDirty := append(s.dirtyByFiles(map[string]bool{file: true}), s.affected(pkgPath)...)
	baseline := errorSet(errorsIn(preDirty))
	created := ""
	var wsBaseline map[string]bool
	if _, err := os.Stat(file); err != nil {
		created = file
		// A created file forces a full workspace reload below, whose
		// diagnostics are workspace-wide rather than dirty-set-scoped;
		// baseline the whole workspace's pre-edit errors for that path so
		// unrelated rot elsewhere doesn't read as new.
		wsBaseline = errorSet(s.errors())
	}
	originals := map[string][]byte{}
	if created == "" {
		originals[file] = src
	}
	if err := os.WriteFile(file, fixed, 0o644); err != nil {
		return nil, err
	}
	undo := func() {
		s.rollback(originals)
		if created != "" {
			os.Remove(created)
		}
	}

	dirty := append(s.dirtyByFiles(map[string]bool{file: true}), s.affected(pkgPath)...)
	if created != "" {
		// A brand-new file changes the package's file set; the incremental
		// path keys off CompiledGoFiles, so take the full reload.
		s.loaded = false
	}
	var diags []Diagnostic
	n := 0
	if s.loaded {
		diags, n, err = s.retypecheck(dirty, baseline)
	} else {
		_, err = s.load()
		diags = filterNew(s.errors(), wsBaseline)
	}
	if err != nil {
		undo()
		return nil, err
	}
	if len(diags) > 0 {
		undo()
		s.loaded = false
		return nil, diagnosticRepairs(&Reject{Reason: "declaration does not typecheck", Diagnostics: diags})
	}
	if _, _, rej := s.findObject(pkgPath, sym); rej != nil {
		undo()
		s.loaded = false
		return nil, &Reject{Reason: "declaration missing after edit", Detail: sym}
	}
	s.noteWrite(file)
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "action": action,
		"file": file, "load_ms": ms, "packages_rechecked": n,
		"generation": s.generation(pkgPath, sym),
	}, baseline)
	s.attachView(res, pkgPath, sym)
	return res, nil
}

// upsertNewPackage creates pkgPath under the module with the declaration as
// its first file.
func (s *Snapshot) upsertNewPackage(pkgPath, text, sym string, loadMS int64) (map[string]any, error) {
	if len(s.pkgs) == 0 || s.pkgs[0].Module == nil {
		return nil, &Reject{Reason: "package not found", Detail: pkgPath}
	}
	mod := s.pkgs[0].Module
	rel, ok := strings.CutPrefix(pkgPath, mod.Path+"/")
	if !ok {
		return nil, &Reject{Reason: "package is outside the module",
			Detail: pkgPath + " not under " + mod.Path}
	}
	dir := filepath.Join(mod.Dir, rel)
	file := filepath.Join(dir, "agent.go")
	if _, err := os.Stat(file); err == nil {
		return nil, &Reject{Reason: "package exists but did not load", Detail: pkgPath}
	}
	src := "package " + filepath.Base(rel) + "\n\n" + strings.TrimSpace(text) + "\n"
	fixed, err := imports.Process(file, []byte(src), nil)
	if err != nil {
		return nil, &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// The new package reloads the whole workspace; baseline its pre-edit
	// errors so unrelated rot elsewhere doesn't read as caused by the new
	// declaration.
	baseline := errorSet(s.errors())
	if err := os.WriteFile(file, fixed, 0o644); err != nil {
		return nil, err
	}
	s.loaded = false
	if _, err := s.load(); err != nil {
		os.Remove(file)
		return nil, err
	}
	if diags := filterNew(s.errors(), baseline); len(diags) > 0 {
		os.Remove(file)
		s.loaded = false
		return nil, diagnosticRepairs(&Reject{Reason: "declaration does not typecheck", Diagnostics: diags})
	}
	if _, _, rej := s.findObject(pkgPath, sym); rej != nil {
		os.Remove(file)
		s.loaded = false
		return nil, &Reject{Reason: "declaration missing after edit", Detail: pkgPath + "." + sym}
	}
	res := addPreExisting(map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "action": "created-package",
		"file": file, "load_ms": loadMS,
	}, baseline)
	s.attachView(res, pkgPath, sym)
	return res, nil
}

// parseDeclText validates that text is exactly one top-level declaration
// and returns its name and symbol address form.
func parseDeclText(text string) (name, sym string, rej *Reject) {
	f, err := parser.ParseFile(token.NewFileSet(), "decl.go", "package _p\n\n"+text, parser.SkipObjectResolution)
	if err != nil {
		return "", "", &Reject{Reason: "declaration does not parse", Detail: err.Error()}
	}
	if len(f.Decls) != 1 {
		return "", "", &Reject{Reason: "expected exactly one top-level declaration",
			Detail: fmt.Sprintf("got %d", len(f.Decls))}
	}
	switch d := f.Decls[0].(type) {
	case *ast.FuncDecl:
		name = d.Name.Name
		if recv := recvTypeName(d); recv != "" {
			return name, recv + "." + name, nil
		}
		return name, name, nil
	case *ast.GenDecl:
		if len(d.Specs) != 1 {
			return "", "", &Reject{Reason: "expected exactly one declaration in the group",
				Detail: fmt.Sprintf("got %d specs", len(d.Specs))}
		}
		switch spec := d.Specs[0].(type) {
		case *ast.TypeSpec:
			return spec.Name.Name, spec.Name.Name, nil
		case *ast.ValueSpec:
			if len(spec.Names) != 1 {
				return "", "", &Reject{Reason: "expected exactly one name", Detail: fmt.Sprint(spec.Names)}
			}
			return spec.Names[0].Name, spec.Names[0].Name, nil
		}
	}
	return "", "", &Reject{Reason: "unsupported declaration kind"}
}

func recvTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	t := d.Recv.List[0].Type
	for {
		switch tt := t.(type) {
		case *ast.StarExpr:
			t = tt.X
		case *ast.IndexExpr:
			t = tt.X
		case *ast.IndexListExpr:
			t = tt.X
		case *ast.Ident:
			return tt.Name
		default:
			return ""
		}
	}
}

// findDeclNode locates an existing standalone top-level declaration by name
// (and receiver for methods): the shared traversal behind findDeclRange
// (the whole range including any doc comment) and set_doc (which needs the
// doc comment and the bare declaration's own start distinctly, to tell
// "replace this existing comment" from "no comment yet, insert one"). A nil
// decl means not found — either genuinely new, or hidden inside a grouped
// block, where the subsequent typecheck rejects the redeclaration with a
// diagnostic naming both sites.
func (s *Snapshot) findDeclNode(p *packages.Package, name, sym string) (filename string, decl ast.Decl, doc *ast.CommentGroup) {
	recv, mname, isMethod := strings.Cut(sym, ".")
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			var dc *ast.CommentGroup
			match := false
			switch d := d.(type) {
			case *ast.FuncDecl:
				if isMethod {
					match = d.Name.Name == mname && recvTypeName(d) == recv
				} else {
					match = d.Recv == nil && d.Name.Name == name
				}
				dc = d.Doc
			case *ast.GenDecl:
				if isMethod || len(d.Specs) != 1 {
					continue
				}
				switch spec := d.Specs[0].(type) {
				case *ast.TypeSpec:
					match = spec.Name.Name == name
				case *ast.ValueSpec:
					match = len(spec.Names) == 1 && spec.Names[0].Name == name
				}
				dc = d.Doc
			}
			if !match {
				continue
			}
			return s.fset.Position(d.Pos()).Filename, d, dc
		}
	}
	return "", nil, nil
}

// findDeclRange returns a declaration's byte range including its doc
// comment, or an empty file when not found. See findDeclNode.
func (s *Snapshot) findDeclRange(p *packages.Package, name, sym string) (string, int, int) {
	file, d, doc := s.findDeclNode(p, name, sym)
	if file == "" {
		return "", 0, 0
	}
	start := d.Pos()
	if doc != nil {
		start = doc.Pos()
	}
	return file, s.fset.Position(start).Offset, s.fset.Position(d.End()).Offset
}
