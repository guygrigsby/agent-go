// Command ago is a spike: a semantic query/mutation CLI for Go workspaces.
//
// Every command emits JSON. Mutations are validated by re-typechecking the
// whole workspace with the edit applied as an overlay; a failed check returns
// a structured rejection and writes nothing.
//
//	ago load [-C dir] [-tests]
//	ago inspect [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago refs    [-C dir] [-tests] -p <pkgpath> -s <Name | Recv.Name>
//	ago set-body [-C dir] -p <pkgpath> -s <Name | Recv.Name> -body-file <f|->
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
	packages.NeedSyntax | packages.NeedTypesInfo

type timing struct {
	LoadMS int64 `json:"load_ms"`
	// ValidateMS is the overlay re-load that checks a mutation.
	ValidateMS int64 `json:"validate_ms,omitempty"`
}

type diagnostic struct {
	Pos string `json:"pos"`
	Msg string `json:"msg"`
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: ago <load|inspect|refs|set-body> [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("C", ".", "workspace directory")
	pkgPath := fs.String("p", "", "package import path")
	sym := fs.String("s", "", "symbol: Name or Recv.Name")
	bodyFile := fs.String("body-file", "", "new function body (- for stdin)")
	tests := fs.Bool("tests", false, "include test packages")
	fs.Parse(args)

	switch cmd {
	case "load":
		runLoad(*dir, *tests)
	case "inspect":
		runInspect(*dir, *pkgPath, *sym)
	case "refs":
		runRefs(*dir, *tests, *pkgPath, *sym)
	case "set-body":
		runSetBody(*dir, *pkgPath, *sym, *bodyFile)
	default:
		fail("unknown command %q", cmd)
	}
}

func load(dir string, tests bool, overlay map[string][]byte) ([]*packages.Package, timing) {
	cfg := &packages.Config{
		Mode:    loadMode,
		Dir:     dir,
		Tests:   tests,
		Overlay: overlay,
	}
	start := time.Now()
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fail("load: %v", err)
	}
	return pkgs, timing{LoadMS: time.Since(start).Milliseconds()}
}

func collectErrors(pkgs []*packages.Package) []diagnostic {
	var diags []diagnostic
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			key := e.Pos + e.Msg
			if !seen[key] {
				seen[key] = true
				diags = append(diags, diagnostic{Pos: e.Pos, Msg: e.Msg})
			}
		}
	})
	return diags
}

func runLoad(dir string, tests bool) {
	pkgs, t := load(dir, tests, nil)
	files := 0
	for _, p := range pkgs {
		files += len(p.CompiledGoFiles)
	}
	emit(map[string]any{
		"status":   "ok",
		"packages": len(pkgs),
		"files":    files,
		"errors":   collectErrors(pkgs),
		"timing":   t,
	})
}

// findObject resolves "Name" or "Recv.Name" in the package's scope.
func findObject(pkgs []*packages.Package, pkgPath, sym string) (*packages.Package, types.Object) {
	var target *packages.Package
	for _, p := range pkgs {
		if p.PkgPath == pkgPath && p.Types != nil {
			target = p
			break
		}
	}
	if target == nil {
		reject("package not found", pkgPath, nil)
	}
	scope := target.Types.Scope()
	recv, name, isMethod := strings.Cut(sym, ".")
	if !isMethod {
		obj := scope.Lookup(sym)
		if obj == nil {
			reject("symbol not found", fmt.Sprintf("%s.%s", pkgPath, sym), nil)
		}
		return target, obj
	}
	recvObj := scope.Lookup(recv)
	if recvObj == nil {
		reject("receiver type not found", fmt.Sprintf("%s.%s", pkgPath, recv), nil)
	}
	obj, _, _ := types.LookupFieldOrMethod(recvObj.Type(), true, target.Types, name)
	if obj == nil {
		reject("method or field not found", fmt.Sprintf("%s.%s", pkgPath, sym), nil)
	}
	return target, obj
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

func runInspect(dir, pkgPath, sym string) {
	pkgs, t := load(dir, false, nil)
	p, obj := findObject(pkgs, pkgPath, sym)
	emit(map[string]any{
		"status":   "ok",
		"name":     obj.Name(),
		"kind":     objKind(obj),
		"type":     types.TypeString(obj.Type(), types.RelativeTo(p.Types)),
		"exported": obj.Exported(),
		"pos":      p.Fset.Position(obj.Pos()).String(),
		"pkg":      pkgPath,
		"timing":   t,
	})
}

func runRefs(dir string, tests bool, pkgPath, sym string) {
	pkgs, t := load(dir, tests, nil)
	_, obj := findObject(pkgs, pkgPath, sym)
	type ref struct {
		Pos string `json:"pos"`
		Pkg string `json:"pkg"`
		Def bool   `json:"def,omitempty"`
	}
	var refs []ref
	seen := map[string]bool{}
	// Object identity is not stable across test/non-test package variants,
	// so match by defining position instead of pointer equality.
	same := func(o types.Object, fset *token.FileSet) bool {
		return o != nil && o.Name() == obj.Name() && o.Pos().IsValid() &&
			fset.Position(o.Pos()).String() == fset.Position(obj.Pos()).String()
	}
	for _, p := range pkgs {
		if p.TypesInfo == nil {
			continue
		}
		add := func(id *ast.Ident, o types.Object, def bool) {
			if !same(o, p.Fset) {
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
	emit(map[string]any{
		"status": "ok",
		"symbol": fmt.Sprintf("%s.%s", pkgPath, sym),
		"count":  len(refs),
		"refs":   refs,
		"timing": t,
	})
}

func runSetBody(dir, pkgPath, sym, bodyFile string) {
	body := readBody(bodyFile)
	pkgs, t := load(dir, false, nil)
	if diags := collectErrors(pkgs); len(diags) > 0 {
		reject("workspace has pre-existing errors", "", diags)
	}
	p, obj := findObject(pkgs, pkgPath, sym)
	fn, ok := obj.(*types.Func)
	if !ok {
		reject("symbol is not a function", objKind(obj), nil)
	}

	decl, file := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		reject("function declaration not found in loaded syntax", sym, nil)
	}
	filename := p.Fset.Position(file.Pos()).Filename
	src, err := os.ReadFile(filename)
	if err != nil {
		fail("read %s: %v", filename, err)
	}

	// Splice at the byte level and reformat; avoids cross-FileSet AST printing.
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
		reject("new body does not parse", err.Error(), nil)
	}

	vstart := time.Now()
	overlay := map[string][]byte{filename: formatted}
	vpkgs, _ := load(dir, false, overlay)
	t.ValidateMS = time.Since(vstart).Milliseconds()
	if diags := collectErrors(vpkgs); len(diags) > 0 {
		reject("edit does not typecheck", "", diags)
	}

	if err := os.WriteFile(filename, formatted, 0o644); err != nil {
		fail("write %s: %v", filename, err)
	}
	emit(map[string]any{
		"status": "accepted",
		"symbol": fmt.Sprintf("%s.%s", pkgPath, sym),
		"file":   filename,
		"timing": t,
	})
}

func findFuncDecl(p *packages.Package, fn *types.Func) (*ast.FuncDecl, *ast.File) {
	for _, file := range p.Syntax {
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Pos() == fn.Pos() {
				return fd, file
			}
		}
	}
	return nil, nil
}

func readBody(bodyFile string) string {
	if bodyFile == "" {
		fail("set-body requires -body-file (use - for stdin)")
	}
	if bodyFile == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail("read stdin: %v", err)
		}
		return string(b)
	}
	b, err := os.ReadFile(bodyFile)
	if err != nil {
		fail("read body: %v", err)
	}
	return string(b)
}

func emit(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func reject(reason, detail string, diags []diagnostic) {
	emit(map[string]any{
		"status":      "rejected",
		"reason":      reason,
		"detail":      detail,
		"diagnostics": diags,
	})
	os.Exit(2)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
