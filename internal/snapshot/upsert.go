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
	if diags := s.errors(); len(diags) > 0 {
		return nil, &Reject{Reason: "workspace has pre-existing errors", Diagnostics: diags}
	}
	name, sym, rej := parseDeclText(text)
	if rej != nil {
		return nil, rej
	}

	p := s.primary(pkgPath)
	if p == nil {
		return s.upsertNewPackage(pkgPath, text, sym, ms)
	}

	file, start, end := s.findDeclRange(p, name, sym)
	action := "replaced"
	var src []byte
	if file == "" {
		action = "added"
		file = filepath.Join(filepath.Dir(p.Fset.Position(p.Syntax[0].Pos()).Filename), "agent.go")
		if b, err := os.ReadFile(file); err == nil {
			src = b
			start, end = len(b), len(b)
		} else {
			src = []byte("package " + p.Types.Name() + "\n")
			start, end = len(src), len(src)
		}
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

	created := ""
	if _, err := os.Stat(file); err != nil {
		created = file
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
		diags, n, err = s.retypecheck(dirty)
	} else {
		_, err = s.load()
		diags = s.errors()
	}
	if err != nil {
		undo()
		return nil, err
	}
	if len(diags) > 0 {
		undo()
		s.loaded = false
		return nil, &Reject{Reason: "declaration does not typecheck", Diagnostics: diags}
	}
	if _, _, rej := s.findObject(pkgPath, sym); rej != nil {
		undo()
		s.loaded = false
		return nil, &Reject{Reason: "declaration missing after edit", Detail: sym}
	}
	s.noteWrite(file)
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "action": action,
		"file": file, "load_ms": ms, "packages_rechecked": n,
	}, nil
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
	if err := os.WriteFile(file, fixed, 0o644); err != nil {
		return nil, err
	}
	s.loaded = false
	if _, err := s.load(); err != nil {
		os.Remove(file)
		return nil, err
	}
	if diags := s.errors(); len(diags) > 0 {
		os.Remove(file)
		s.loaded = false
		return nil, &Reject{Reason: "declaration does not typecheck", Diagnostics: diags}
	}
	if _, _, rej := s.findObject(pkgPath, sym); rej != nil {
		os.Remove(file)
		s.loaded = false
		return nil, &Reject{Reason: "declaration missing after edit", Detail: pkgPath + "." + sym}
	}
	return map[string]any{
		"status": "accepted", "symbol": pkgPath + "." + sym, "action": "created-package",
		"file": file, "load_ms": loadMS,
	}, nil
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

// findDeclRange locates an existing standalone top-level declaration by
// name (and receiver for methods), returning its file and byte range
// including the doc comment. Empty file means not found — either genuinely
// new, or hidden inside a grouped block, where the subsequent typecheck
// rejects the redeclaration with a diagnostic naming both sites.
func (s *Snapshot) findDeclRange(p *packages.Package, name, sym string) (string, int, int) {
	recv, mname, isMethod := strings.Cut(sym, ".")
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			var doc *ast.CommentGroup
			match := false
			switch d := d.(type) {
			case *ast.FuncDecl:
				if isMethod {
					match = d.Name.Name == mname && recvTypeName(d) == recv
				} else {
					match = d.Recv == nil && d.Name.Name == name
				}
				doc = d.Doc
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
				doc = d.Doc
			}
			if !match {
				continue
			}
			start := d.Pos()
			if doc != nil {
				start = doc.Pos()
			}
			pos := s.fset.Position(start)
			return pos.Filename, pos.Offset, s.fset.Position(d.End()).Offset
		}
	}
	return "", 0, 0
}
