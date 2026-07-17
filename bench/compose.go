package bench

// Oracle patch composition: the recipes behind the add-param tasks that a
// single sequential add-param cannot replay. Two shapes exist in the mined
// corpus:
//
//   - interface tasks (boundary 9354a4eb): the ground truth widens an
//     interface method and every implementor in the same commit. Replayed as
//     ONE atomic patch of set_signature ops — the interface method first
//     (discovered from a rejected implementor via the implementations
//     query), then every implementing type, then any test-only implementors
//     the dry-run typecheck reveals.
//
//   - func-as-value tasks (boundary fb1ea92a, vault 7103bc2c): the spec'd
//     functions are assigned to a named func type (a registry), so add_param
//     rejects their value uses. Replayed as ONE atomic patch: set_signature
//     per spec'd function, the named type widened via upsert_decl, and the
//     fallout — calls of type-typed values, func literals feeding the type —
//     repaired with set_body ops built by textual surgery on the pristine
//     source, iterated under dry_run until the patch typechecks.
//
// Composition talks to ago only through composeEnv, so the pure helpers
// stay unit-testable without a workspace or daemon.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

// composeEnv is the protocol access composition needs. call runs one ago
// verb; patch posts one patch envelope. Both return the decoded response
// (nil on transport failure) and are expected to record the transcript.
type composeEnv struct {
	wt    string
	mod   string
	call  func(args ...string) map[string]any
	patch func(env map[string]any) map[string]any
}

// maxComposeRounds bounds the dry-run repair loop; every observed task
// converges in two or three rounds, anything past this is a finding.
const maxComposeRounds = 8

// sigOfInspect returns inspect's "type" string for pkg.sym, "" when the
// symbol does not resolve.
func (o composeEnv) sigOfInspect(pkg, sym string) string {
	out := o.call("inspect", "-p", pkg, "-s", sym)
	if out == nil || out["status"] != "ok" {
		return ""
	}
	s, _ := out["type"].(string)
	return s
}

// posOfInspect returns the declaration position "file:line:col", "" when
// the symbol does not resolve.
func (o composeEnv) posOfInspect(pkg, sym string) string {
	out := o.call("inspect", "-p", pkg, "-s", sym)
	if out == nil || out["status"] != "ok" {
		return ""
	}
	s, _ := out["pos"].(string)
	return s
}

// composeAddParam replays an add-param task whose sequential replay
// rejected. rejection is the decoded rejection that triggered the
// fallback; it selects the recipe.
func composeAddParam(o composeEnv, t Manifest, rejection map[string]any) error {
	blob := fmt.Sprint(rejection)
	switch {
	case strings.Contains(blob, "wrong type for method"):
		return composeInterfaceAddParam(o, t)
	case strings.Contains(blob, "used as a value"):
		return composeValueUseAddParam(o, t)
	}
	return fmt.Errorf("oracle add-param: no composition recipe for rejection %v", rejection)
}

// pickAddedParam votes the parameter the interface method itself gains:
// the majority type across specs, named by the first spec that gave it a
// real name (an all-underscore majority stays "_").
func pickAddedParam(specs []AddParamSpec) (string, string) {
	count := map[string]int{}
	for _, a := range specs {
		count[a.Type]++
	}
	typ, best := "", 0
	for k, n := range count {
		if n > best {
			typ, best = k, n
		}
	}
	name := "_"
	for _, a := range specs {
		if a.Type == typ && a.Name != "_" {
			name = a.Name
			break
		}
	}
	return name, typ
}

// composeInterfaceAddParam builds the one-patch interface replay.
func composeInterfaceAddParam(o composeEnv, t Manifest) error {
	addName, addType := pickAddedParam(t.AddParams)

	// Discover the interface through a spec'd implementor: the receiver's
	// satisfied interfaces, filtered to those declaring the method.
	var ifacePkg, ifaceSym, method string
	for _, a := range t.AddParams {
		recv, m, ok := strings.Cut(a.Sym, ".")
		if !ok {
			continue
		}
		impls := o.call("query", "--kind", "implementations", "-p", a.Pkg, "-s", recv)
		if impls == nil || impls["status"] != "ok" {
			continue
		}
		ifaces, _ := impls["interfaces"].([]any)
		for _, raw := range ifaces {
			h, _ := raw.(map[string]any)
			ip, _ := h["pkg"].(string)
			is, _ := h["sym"].(string)
			if o.sigOfInspect(ip, is+"."+m) != "" {
				ifacePkg, ifaceSym, method = ip, is, m
				break
			}
		}
		if ifaceSym != "" {
			break
		}
	}
	if ifaceSym == "" {
		return fmt.Errorf("oracle interface replay: no workspace interface declares the spec'd method")
	}

	// One set_signature per declaration: the interface method, then every
	// implementing type's method (workspace scan plus manifest union — the
	// scan sees only primary package variants, the manifest only what the
	// miner extracted).
	specBySym := map[string]AddParamSpec{}
	for _, a := range t.AddParams {
		specBySym[a.Pkg+"."+a.Sym] = a
	}
	type target struct{ pkg, sym string }
	targets := []target{}
	seen := map[string]bool{}
	add := func(pkg, sym string) {
		if !seen[pkg+"."+sym] {
			seen[pkg+"."+sym] = true
			targets = append(targets, target{pkg, sym})
		}
	}
	impls := o.call("query", "--kind", "implementations", "-p", ifacePkg, "-s", ifaceSym)
	if impls != nil {
		hits, _ := impls["types"].([]any)
		for _, raw := range hits {
			h, _ := raw.(map[string]any)
			pkg, _ := h["pkg"].(string)
			sym, _ := h["sym"].(string)
			add(pkg, sym+"."+method)
		}
	}
	for _, a := range t.AddParams {
		add(a.Pkg, a.Sym)
	}

	ifaceSig, err := o.sourceSig(ifacePkg, ifaceSym+"."+method)
	if err != nil {
		return err
	}
	ifaceNew, err := appendParamToSig(ifaceSig, addName, addType)
	if err != nil {
		return err
	}
	ops := []map[string]any{{
		"op": "set_signature", "pkg": ifacePkg, "sym": ifaceSym + "." + method,
		"signature": ifaceNew, "defaults": map[string]string{addName: zeroExpr(addType)},
	}}
	for _, tg := range targets {
		op, err := o.implementorOp(tg.pkg, tg.sym, specBySym[tg.pkg+"."+tg.sym], addType)
		if err != nil {
			return err
		}
		if op != nil {
			ops = append(ops, op)
		}
	}

	// Dry-run repair loop: test-only implementors are invisible to the
	// implementations scan (primary variants only) and surface here.
	for range maxComposeRounds {
		res := o.patch(map[string]any{"pkg": ifacePkg, "sym": ifaceSym + "." + method,
			"dry_run": true, "ops": ops})
		if res != nil && res["status"] == "ok" {
			final := o.patch(map[string]any{"pkg": ifacePkg, "sym": ifaceSym + "." + method, "ops": ops})
			if final != nil && final["status"] == "accepted" {
				return nil
			}
			return fmt.Errorf("oracle interface patch: %v", final)
		}
		added := false
		for _, d := range diagnosticsOf(res) {
			typ, meth, ok := wrongTypeForMethod(d.msg)
			if !ok || meth != method {
				continue
			}
			pkg := pkgPathFor(o.wt, o.mod, strings.SplitN(d.pos, ":", 2)[0])
			if seen[pkg+"."+typ+"."+method] {
				continue
			}
			seen[pkg+"."+typ+"."+method] = true
			op, err := o.implementorOp(pkg, typ+"."+method, AddParamSpec{}, addType)
			if err != nil {
				return err
			}
			if op != nil {
				ops = append(ops, op)
				added = true
			}
		}
		if !added {
			return fmt.Errorf("oracle interface patch stuck: %v", res)
		}
	}
	return fmt.Errorf("oracle interface patch: no convergence in %d rounds", maxComposeRounds)
}

// implementorOp builds one implementor's set_signature op: its current
// source signature plus the interface's added parameter. When the manifest
// spec names a parameter of a DIFFERENT type (the miner saw a rename, e.g.
// `_ context.Context` becoming `ctx context.Context` in the same commit),
// the existing parameter of that type is renamed too, so one op satisfies
// both the interface and the spec. Returns nil when the signature already
// carries the addition (a spec the sequential pass had applied).
func (o composeEnv) implementorOp(pkg, sym string, spec AddParamSpec, addType string) (map[string]any, error) {
	name := "_"
	if spec.Name != "" && spec.Type == addType {
		name = spec.Name
	}
	if cur := o.sigOfInspect(pkg, sym); cur != "" && sigHasParam(cur, name, addType) {
		return nil, nil
	}
	sig, err := o.sourceSig(pkg, sym)
	if err != nil {
		return nil, err
	}
	defaults := map[string]string{name: zeroExpr(addType)}
	if spec.Name != "" && spec.Type != addType {
		renamed, err := renameParamOfType(sig, spec.Type, spec.Name)
		if err != nil {
			return nil, err
		}
		if renamed != sig {
			sig = renamed
			defaults[spec.Name] = zeroExpr(spec.Type)
		}
	}
	newSig, err := appendParamToSig(sig, name, addType)
	if err != nil {
		return nil, err
	}
	return map[string]any{"op": "set_signature", "pkg": pkg, "sym": sym,
		"signature": newSig, "defaults": defaults}, nil
}

// composeValueUseAddParam builds the one-patch func-as-value replay.
func composeValueUseAddParam(o composeEnv, t Manifest) error {
	if len(t.AddParams) == 0 {
		return fmt.Errorf("oracle value-use replay: no specs")
	}
	addType := t.AddParams[0].Type

	var ops []map[string]any
	anchor := t.AddParams[0]
	for _, a := range t.AddParams {
		if cur := o.sigOfInspect(a.Pkg, a.Sym); cur != "" && sigHasParam(cur, a.Name, a.Type) {
			continue // the sequential pass already landed this one
		}
		sig, err := o.sourceSig(a.Pkg, a.Sym)
		if err != nil {
			return err
		}
		newSig, err := appendParamToSig(sig, a.Name, a.Type)
		if err != nil {
			return err
		}
		ops = append(ops, map[string]any{"op": "set_signature", "pkg": a.Pkg, "sym": a.Sym,
			"signature": newSig, "defaults": map[string]string{a.Name: zeroExpr(a.Type)}})
	}

	repairs := newRepairSet(o.wt, o.mod)
	typePatched := map[string]bool{}
	var typeOps []map[string]any
	for range maxComposeRounds {
		all := append(append([]map[string]any{}, ops...), typeOps...)
		all = append(all, repairs.ops()...)
		res := o.patch(map[string]any{"pkg": anchor.Pkg, "sym": anchor.Sym, "dry_run": true, "ops": all})
		if res != nil && res["status"] == "ok" {
			final := o.patch(map[string]any{"pkg": anchor.Pkg, "sym": anchor.Sym, "ops": all})
			if final != nil && final["status"] == "accepted" {
				return nil
			}
			return fmt.Errorf("oracle value-use patch: %v", final)
		}
		progressed := false
		for _, d := range diagnosticsOf(res) {
			file := strings.SplitN(d.pos, ":", 2)[0]
			src, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			filePkg := pkgPathFor(o.wt, o.mod, file)
			switch {
			case reNotEnoughArgs.MatchString(d.msg):
				callee := reNotEnoughArgs.FindStringSubmatch(d.msg)[1]
				if err := repairs.appendArgToCalls(src, file, filePkg, callee, zeroExpr(addType)); err == nil {
					progressed = true
				}
			case reVarAsValue.MatchString(d.msg):
				m := reVarAsValue.FindStringSubmatch(d.msg)
				if err := repairs.appendParamToVarLiterals(src, file, filePkg, m[1], addType); err == nil {
					progressed = true
				}
				progressed = o.ensureTypeOp(m[2], src, filePkg, addType, typePatched, &typeOps) || progressed
			case reLitAsValue.MatchString(d.msg):
				m := reLitAsValue.FindStringSubmatch(d.msg)
				line := lineOfPos(d.pos)
				if err := repairs.appendParamToLiteralAt(src, file, filePkg, line, addType); err == nil {
					progressed = true
				}
				progressed = o.ensureTypeOp(m[1], src, filePkg, addType, typePatched, &typeOps) || progressed
			case reFuncAsValue.MatchString(d.msg):
				m := reFuncAsValue.FindStringSubmatch(d.msg)
				progressed = o.ensureTypeOp(m[2], src, filePkg, addType, typePatched, &typeOps) || progressed
			}
		}
		if !progressed {
			return fmt.Errorf("oracle value-use patch stuck: %v", res)
		}
	}
	return fmt.Errorf("oracle value-use patch: no convergence in %d rounds", maxComposeRounds)
}

// Typecheck message shapes the repair loop acts on. The named-function and
// func-literal forms both name the target type ("as audit.Factory value"),
// which is what identifies the registry type to widen.
var (
	reNotEnoughArgs = regexp.MustCompile(`not enough arguments in call to ([\w.]+)`)
	reVarAsValue    = regexp.MustCompile(`cannot use (\w+) \(variable of type func.* as ([\w.]+) value`)
	reLitAsValue    = regexp.MustCompile(`cannot use (?:func literal|\(func\(.*\) literal\)) .* as ([\w.]+) value`)
	reFuncAsValue   = regexp.MustCompile(`cannot use ([\w.]+) \(value of type func.* as ([\w.]+) value`)
)

// ensureTypeOp adds the upsert_decl widening the named func type, once.
// ref is the type as the diagnostic spells it ("Handler", "audit.Factory"),
// resolved against the diagnostic file's imports.
func (o composeEnv) ensureTypeOp(ref string, src []byte, filePkg, addType string, done map[string]bool, ops *[]map[string]any) bool {
	pkg, name, err := resolveTypeRef(ref, src, filePkg)
	if err != nil || done[pkg+"."+name] {
		return false
	}
	pos := o.posOfInspect(pkg, name)
	if pos == "" {
		return false
	}
	declFile := strings.SplitN(pos, ":", 2)[0]
	declSrc, err := os.ReadFile(declFile)
	if err != nil {
		return false
	}
	text, err := appendTypeToFuncTypeDecl(declSrc, declFile, name, addType)
	if err != nil {
		return false
	}
	done[pkg+"."+name] = true
	*ops = append(*ops, map[string]any{"op": "upsert_decl", "pkg": pkg, "text": text})
	return true
}

type diag struct{ pos, msg string }

func diagnosticsOf(res map[string]any) []diag {
	if res == nil {
		return nil
	}
	raw, _ := res["diagnostics"].([]any)
	var out []diag
	for _, r := range raw {
		m, _ := r.(map[string]any)
		pos, _ := m["pos"].(string)
		msg, _ := m["msg"].(string)
		out = append(out, diag{pos, msg})
	}
	return out
}

var reWrongType = regexp.MustCompile(`\*?(\w+) does not implement [\w./]+ \(wrong type for method (\w+)\)`)

func wrongTypeForMethod(msg string) (typ, method string, ok bool) {
	m := reWrongType.FindStringSubmatch(msg)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func lineOfPos(pos string) int {
	parts := strings.Split(pos, ":")
	if len(parts) < 2 {
		return 0
	}
	n := 0
	fmt.Sscanf(parts[1], "%d", &n)
	return n
}

// pkgPathFor maps a file inside the worktree to its import path in the
// root module. Nested modules (a checked-in sdk/ with its own go.mod) are
// outside this recipe's reach and would resolve wrong here; the tasks this
// composition serves live in the root module.
func pkgPathFor(wt, mod, file string) string {
	rel := strings.TrimPrefix(file, strings.TrimSuffix(wt, "/")+"/")
	dir := path.Dir(rel)
	if dir == "." {
		return mod
	}
	return mod + "/" + dir
}

// moduleOf reads the module path from the worktree's go.mod.
func moduleOf(wt string) string {
	raw, err := os.ReadFile(path.Join(wt, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// sourceSig renders pkg.sym's signature as source text "(params) results"
// exactly as its declaring file spells it — inspect's types-qualified
// rendering is not valid source. Methods on interfaces resolve to the
// interface field's func type.
func (o composeEnv) sourceSig(pkg, sym string) (string, error) {
	pos := o.posOfInspect(pkg, sym)
	if pos == "" {
		return "", fmt.Errorf("sourceSig: %s.%s does not resolve", pkg, sym)
	}
	file := strings.SplitN(pos, ":", 2)[0]
	line := lineOfPos(pos)
	src, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		return "", err
	}
	var ft *ast.FuncType
	ast.Inspect(f, func(n ast.Node) bool {
		if ft != nil {
			return false
		}
		switch d := n.(type) {
		case *ast.FuncDecl:
			if fset.Position(d.Name.Pos()).Line == line {
				ft = d.Type
				return false
			}
		case *ast.InterfaceType:
			for _, fld := range d.Methods.List {
				for _, id := range fld.Names {
					if fset.Position(id.Pos()).Line == line {
						if t, ok := fld.Type.(*ast.FuncType); ok {
							ft = t
							return false
						}
					}
				}
			}
		}
		return true
	})
	if ft == nil {
		return "", fmt.Errorf("sourceSig: no func type at %s", pos)
	}
	return funcTypeSource(fset, src, ft), nil
}

// funcTypeSource renders a FuncType's "(params) results" source text.
func funcTypeSource(fset *token.FileSet, src []byte, ft *ast.FuncType) string {
	start := fset.Position(ft.Params.Opening).Offset
	end := fset.Position(ft.Params.Closing).Offset + 1
	sig := string(src[start:end])
	if ft.Results != nil {
		rs := fset.Position(ft.Results.Pos()).Offset
		re := fset.Position(ft.Results.End()).Offset
		sig += " " + string(src[rs:re])
	}
	return sig
}

// parseSigText parses "(params) results" by wrapping it in a throwaway
// func declaration, returning the wrapper offset so positions map back
// onto the original text.
func parseSigText(sig string) (*token.FileSet, *ast.FuncType, int, error) {
	const prefix = "package p\nfunc _x"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sig.go", prefix+sig+" {}", parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("signature %q does not parse: %v", sig, err)
	}
	fd, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		return nil, nil, 0, fmt.Errorf("signature %q does not parse", sig)
	}
	return fset, fd.Type, len(prefix), nil
}

// appendParamToSig appends "name typ" to a signature's parameter list,
// before a final variadic parameter when one exists (nothing may follow a
// variadic).
func appendParamToSig(sig, name, typ string) (string, error) {
	fset, ft, off, err := parseSigText(sig)
	if err != nil {
		return "", err
	}
	insert := fset.Position(ft.Params.Closing).Offset - off
	text := ", " + name + " " + typ
	if ft.Params.NumFields() == 0 {
		text = name + " " + typ
	} else if last := ft.Params.List[len(ft.Params.List)-1]; last != nil {
		if _, variadic := last.Type.(*ast.Ellipsis); variadic {
			insert = fset.Position(last.Pos()).Offset - off
			text = name + " " + typ + ", "
		}
	}
	return sig[:insert] + text + sig[insert:], nil
}

// renameParamOfType renames the first parameter of the given source type
// whose name differs from newName. A signature with no such parameter is
// returned unchanged — the caller treats that as "nothing to carry".
func renameParamOfType(sig, typ, newName string) (string, error) {
	fset, ft, off, err := parseSigText(sig)
	if err != nil {
		return "", err
	}
	for _, fld := range ft.Params.List {
		text := sig[fset.Position(fld.Type.Pos()).Offset-off : fset.Position(fld.Type.End()).Offset-off]
		if text != typ {
			continue
		}
		if len(fld.Names) == 0 {
			// Unnamed parameter: name it in place.
			at := fset.Position(fld.Type.Pos()).Offset - off
			return sig[:at] + newName + " " + sig[at:], nil
		}
		for _, id := range fld.Names {
			if id.Name == newName {
				return sig, nil
			}
		}
		if len(fld.Names) == 1 {
			s := fset.Position(fld.Names[0].Pos()).Offset - off
			e := fset.Position(fld.Names[0].End()).Offset - off
			return sig[:s] + newName + sig[e:], nil
		}
	}
	return sig, nil
}

// appendTypeToFuncTypeDecl returns the full source text (doc comment
// included) of the named `type N func(...)` declaration in src, with typ
// appended to its parameter list.
func appendTypeToFuncTypeDecl(src []byte, filename, name, typ string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return "", err
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, sp := range gd.Specs {
			ts, ok := sp.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			ft, ok := ts.Type.(*ast.FuncType)
			if !ok {
				return "", fmt.Errorf("%s is not a func type", name)
			}
			start := fset.Position(gd.Pos()).Offset
			if gd.Doc != nil {
				start = fset.Position(gd.Doc.Pos()).Offset
			}
			end := fset.Position(gd.End()).Offset
			insert := fset.Position(ft.Params.Closing).Offset
			text := ", " + typ
			if ft.Params.NumFields() == 0 {
				text = typ
			}
			return string(src[start:insert]) + text + string(src[insert:end]), nil
		}
	}
	return "", fmt.Errorf("type %s not found in %s", name, filename)
}

// resolveTypeRef resolves a diagnostic's type reference ("Handler" or
// "audit.Factory") against the diagnostic file: unqualified names live in
// the file's own package, qualified ones resolve through its imports
// (aliases included).
func resolveTypeRef(ref string, src []byte, filePkg string) (pkg, name string, err error) {
	qual, base, qualified := strings.Cut(ref, ".")
	if !qualified {
		return filePkg, ref, nil
	}
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "ref.go", src, parser.ImportsOnly)
	if perr != nil {
		return "", "", perr
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		local := path.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == qual {
			return p, base, nil
		}
	}
	return "", "", fmt.Errorf("qualifier %q not among imports", qual)
}

// repairSet accumulates textual repairs per enclosing declaration and
// renders them as set_body ops. Every edit is a pure insertion at a byte
// offset of the PRISTINE file, so repairs from several dry-run rounds
// compose without tracking how earlier ops shift the text.
type repairSet struct {
	wt, mod string
	decls   map[string]*declRepair
}

type declRepair struct {
	pkg, sym string
	src      []byte
	bodyLbr  int            // offset of the body's opening brace
	bodyRbr  int            // offset of the body's closing brace
	edits    map[int]string // insertion offset -> text
}

func newRepairSet(wt, mod string) *repairSet {
	return &repairSet{wt: wt, mod: mod, decls: map[string]*declRepair{}}
}

// ops renders the accumulated repairs, one set_body per touched decl,
// deterministic order.
func (rs *repairSet) ops() []map[string]any {
	keys := make([]string, 0, len(rs.decls))
	for k := range rs.decls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []map[string]any
	for _, k := range keys {
		d := rs.decls[k]
		offs := make([]int, 0, len(d.edits))
		for o := range d.edits {
			offs = append(offs, o)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(offs)))
		body := string(d.src[d.bodyLbr+1 : d.bodyRbr])
		for _, o := range offs {
			at := o - (d.bodyLbr + 1)
			body = body[:at] + d.edits[o] + body[at:]
		}
		out = append(out, map[string]any{"op": "set_body", "pkg": d.pkg, "sym": d.sym,
			"body": strings.TrimRight(strings.TrimPrefix(body, "\n"), "\n\t ")})
	}
	return out
}

// add registers an insertion inside the FuncDecl enclosing offset.
func (rs *repairSet) add(fset *token.FileSet, f *ast.File, src []byte, pkg string, at int, text string) error {
	var fd *ast.FuncDecl
	for _, d := range f.Decls {
		d, ok := d.(*ast.FuncDecl)
		if !ok || d.Body == nil {
			continue
		}
		s := fset.Position(d.Body.Lbrace).Offset
		e := fset.Position(d.Body.Rbrace).Offset
		if at > s && at < e {
			fd = d
			break
		}
	}
	if fd == nil {
		return fmt.Errorf("no enclosing function body at offset %d", at)
	}
	sym := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		t := fd.Recv.List[0].Type
		if se, ok := t.(*ast.StarExpr); ok {
			t = se.X
		}
		if id, ok := t.(*ast.Ident); ok {
			sym = id.Name + "." + fd.Name.Name
		}
	}
	key := pkg + "." + sym
	d := rs.decls[key]
	if d == nil {
		d = &declRepair{pkg: pkg, sym: sym, src: src,
			bodyLbr: fset.Position(fd.Body.Lbrace).Offset,
			bodyRbr: fset.Position(fd.Body.Rbrace).Offset,
			edits:   map[int]string{}}
		rs.decls[key] = d
	}
	d.edits[at] = text
	return nil
}

func parseRepairFile(src []byte, filename string) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	return fset, f, err
}

// appendArgToCalls appends argText to every call of callee (rendered
// source text of the call's Fun) in the file. Matching is by name rather
// than position: after a set_body repair has been queued, later dry-run
// rounds report positions against patched content that no longer maps onto
// the pristine bytes this set edits.
func (rs *repairSet) appendArgToCalls(src []byte, filename, pkg, callee, argText string) error {
	fset, f, err := parseRepairFile(src, filename)
	if err != nil {
		return err
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		s := fset.Position(call.Fun.Pos()).Offset
		e := fset.Position(call.Fun.End()).Offset
		if string(src[s:e]) != callee {
			return true
		}
		at := fset.Position(call.Rparen).Offset
		text := ", " + argText
		if len(call.Args) == 0 {
			text = argText
		}
		if rs.add(fset, f, src, pkg, at, text) == nil {
			found = true
		}
		return true
	})
	if !found {
		return fmt.Errorf("no call of %s in %s", callee, filename)
	}
	return nil
}

// appendParamToVarLiterals widens every func literal bound to varName in
// the file (fn := func(...) {...} and var fn = func(...) {...} forms).
func (rs *repairSet) appendParamToVarLiterals(src []byte, filename, pkg, varName, typ string) error {
	fset, f, err := parseRepairFile(src, filename)
	if err != nil {
		return err
	}
	found := false
	visit := func(names []ast.Expr, values []ast.Expr) {
		for i, lhs := range names {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != varName || i >= len(values) {
				continue
			}
			lit, ok := values[i].(*ast.FuncLit)
			if !ok {
				continue
			}
			if rs.addLitParam(fset, f, src, pkg, lit, typ) == nil {
				found = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			visit(st.Lhs, st.Rhs)
		case *ast.ValueSpec:
			exprs := make([]ast.Expr, len(st.Names))
			for i, id := range st.Names {
				exprs[i] = id
			}
			visit(exprs, st.Values)
		}
		return true
	})
	if !found {
		return fmt.Errorf("no func literal bound to %s in %s", varName, filename)
	}
	return nil
}

// appendParamToLiteralAt widens the func literal that starts on the given
// line (positions are trustworthy for literal diagnostics: they surface on
// the first dry-run round after the type op lands, before any set_body
// repair has shifted lines).
func (rs *repairSet) appendParamToLiteralAt(src []byte, filename, pkg string, line int, typ string) error {
	fset, f, err := parseRepairFile(src, filename)
	if err != nil {
		return err
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || fset.Position(lit.Pos()).Line != line {
			return true
		}
		if rs.addLitParam(fset, f, src, pkg, lit, typ) == nil {
			found = true
		}
		return false
	})
	if !found {
		return fmt.Errorf("no func literal on line %d of %s", line, filename)
	}
	return nil
}

// addLitParam appends the widening parameter to one literal's list: named
// lists gain "_ typ", unnamed ones the bare type.
func (rs *repairSet) addLitParam(fset *token.FileSet, f *ast.File, src []byte, pkg string, lit *ast.FuncLit, typ string) error {
	at := fset.Position(lit.Type.Params.Closing).Offset
	named := false
	for _, fld := range lit.Type.Params.List {
		if len(fld.Names) > 0 {
			named = true
		}
	}
	text := typ
	if named {
		text = "_ " + typ
	}
	if lit.Type.Params.NumFields() > 0 {
		text = ", " + text
	}
	return rs.add(fset, f, src, pkg, at, text)
}
