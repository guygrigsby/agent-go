package snapshot

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/imports"
)

// Test ops (docs/specs/language.md "Test ops"): add_test scaffolds a
// table-driven test from a target function's signature; add_test_case,
// set_test_case, and remove_test_case manage its case rows. All four are
// decl ops (declOps, patch.go) — they address their own pkg/target/test
// rather than a statement position inside the envelope's fixed function.

// testFileConvention scans every "_test.go" already in dir (not just the
// one add_test is about to create or extend) for the package's established
// convention: external test package (a precedent of "package <pkgName>_test")
// and testify usage (a precedent of importing testify's require). No
// existing test file means no precedent — internal package, stdlib
// assertions — matching the spec's "no precedent" default.
func testFileConvention(dir, pkgName string) (extPkg, testify bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		src := string(b)
		if strings.Contains(src, "package "+pkgName+"_test") {
			extPkg = true
		}
		if strings.Contains(src, `"github.com/stretchr/testify/require"`) {
			testify = true
		}
	}
	return extPkg, testify
}

// addTestFileEdit resolves add_test's target and where its scaffold lands:
// <declfile>_test.go next to the target's own declaration, mirroring
// upsertDeclEdit's locate-or-append core (upsertDeclEdit itself always
// targets the package's own agent.go; add_test needs an explicit,
// target-derived file instead, hence a dedicated variant rather than reuse).
// needsCreate is true when that file doesn't exist yet — the common case for
// a package's first generated test.
func addTestFileEdit(s *Snapshot, pkgPath, target string) (fn *types.Func, testFile string, insertOffset int, needsCreate bool, pkgName string, extPkg, testify bool, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, target)
	if rej0 != nil {
		return nil, "", 0, false, "", false, false, rej0
	}
	f, ok := obj.(*types.Func)
	if !ok {
		return nil, "", 0, false, "", false, false, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	if f.Signature().Recv() != nil {
		return nil, "", 0, false, "", false, false, &Reject{
			Reason: "add_test targets a plain function in v1; methods are not yet supported", Detail: target}
	}
	decl, declFile := findFuncDecl(p, f)
	if decl == nil {
		return nil, "", 0, false, "", false, false, &Reject{Reason: "function declaration not found", Detail: target}
	}
	testFile = strings.TrimSuffix(declFile, ".go") + "_test.go"
	pkgName = p.Types.Name()
	extPkg, testify = testFileConvention(filepath.Dir(declFile), pkgName)
	if b, err := os.ReadFile(testFile); err == nil {
		return f, testFile, len(b), false, pkgName, extPkg, testify, nil
	}
	return f, testFile, 0, true, pkgName, extPkg, testify, nil
}

// placeholders renders n "%v" verbs joined for a t.Errorf format string, so
// the generated message's verb count always matches its argument count
// (go test's printf vet check enforces this) regardless of the target's
// arity.
func placeholders(n int) string {
	v := make([]string, n)
	for i := range v {
		v[i] = "%v"
	}
	return strings.Join(v, ", ")
}

// joinArgs comma-joins non-empty parts, so a zero-arg target's empty
// argList doesn't leave a stray leading ", ".
func joinArgs(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// renderCompare renders the t.Run body: the call, its result assignment,
// and the comparison(s). The single-result-no-error shape is the spec's
// canonical byte-for-byte skeleton (docs/specs/language.md, task-10 brief);
// every other arity (0 results, an error result, 2+ results) composes the
// same got/want-per-field pattern the canonical shape specializes, per the
// spec's "multi-result targets compare each" and "error results get wantErr
// bool" notes.
func renderCompare(callee string, callArgs, gotNames, wantNames []string, hasErr, testify bool) string {
	argList := strings.Join(callArgs, ", ")
	ph := placeholders(len(callArgs))
	call := callee + "(" + argList + ")"

	if len(gotNames) == 1 && !hasErr {
		if testify {
			return fmt.Sprintf("got := %s\nrequire.Equal(t, tt.want, got)", call)
		}
		msg := callee + "(" + ph + ") = %v, want %v"
		return fmt.Sprintf("if got := %s; got != tt.want {\nt.Errorf(%s, %s)\n}",
			call, strconv.Quote(msg), joinArgs(argList, "got, tt.want"))
	}

	var lhs []string
	lhs = append(lhs, gotNames...)
	if hasErr {
		lhs = append(lhs, "err")
	}
	if len(lhs) == 0 {
		return call
	}

	var b strings.Builder
	b.WriteString(strings.Join(lhs, ", ") + " := " + call + "\n")
	if hasErr {
		if testify {
			b.WriteString("require.Equal(t, tt.wantErr, err != nil)\n")
		} else {
			msg := callee + "(" + ph + ") error = %v, wantErr %v"
			fmt.Fprintf(&b, "if (err != nil) != tt.wantErr {\nt.Errorf(%s, %s)\nreturn\n}\n",
				strconv.Quote(msg), joinArgs(argList, "err, tt.wantErr"))
		}
	}
	for i, g := range gotNames {
		w := wantNames[i]
		if testify {
			fmt.Fprintf(&b, "require.Equal(t, tt.%s, %s)\n", w, g)
		} else {
			msg := callee + "(" + ph + ") " + g + " = %v, want %v"
			fmt.Fprintf(&b, "if %s != tt.%s {\nt.Errorf(%s, %s)\n}\n",
				g, w, strconv.Quote(msg), joinArgs(argList, g+", tt."+w))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTestFunc builds the full TestXxx source text: the case struct
// derived from fn's signature (params -> named fields, else v0/v1...;
// non-error results -> want/want1...; a trailing error result -> wantErr
// bool), the rows slice, and the range+t.Run loop. Non-comparable results
// (slice/map/func-typed) reject rather than generate an uncompilable
// comparison.
func renderTestFunc(testName, target string, fn *types.Func, extPkg bool, pkgName string, testify bool) (string, *Reject) {
	sig := fn.Signature()
	params := sig.Params()
	results := sig.Results()
	errType := types.Universe.Lookup("error").Type()
	hasErr := results.Len() > 0 && types.Identical(results.At(results.Len()-1).Type(), errType)
	nonErr := results.Len()
	if hasErr {
		nonErr--
	}
	for i := range nonErr {
		rt := results.At(i).Type()
		if !types.Comparable(rt) {
			return "", &Reject{Reason: "result type not comparable; write the test with upsert_decl",
				Detail: types.TypeString(rt, types.RelativeTo(fn.Pkg()))}
		}
	}

	callee := target
	if extPkg {
		callee = pkgName + "." + target
	}

	fields := []string{"name string"}
	var callArgs []string
	for i := range params.Len() {
		v := params.At(i)
		name := v.Name()
		if name == "" || name == "_" {
			name = fmt.Sprintf("v%d", i)
		}
		callArgs = append(callArgs, "tt."+name)
		fields = append(fields, name+" "+types.TypeString(v.Type(), types.RelativeTo(fn.Pkg())))
	}
	var gotNames, wantNames []string
	for i := range nonErr {
		want, got := "want", "got"
		if i > 0 {
			want, got = fmt.Sprintf("want%d", i), fmt.Sprintf("got%d", i)
		}
		gotNames = append(gotNames, got)
		wantNames = append(wantNames, want)
		fields = append(fields, want+" "+types.TypeString(results.At(i).Type(), types.RelativeTo(fn.Pkg())))
	}
	if hasErr {
		fields = append(fields, "wantErr bool")
	}

	body := renderCompare(callee, callArgs, gotNames, wantNames, hasErr, testify)

	var b strings.Builder
	fmt.Fprintf(&b, "func %s(t *testing.T) {\n", testName)
	b.WriteString("tests := []struct {\n")
	for _, f := range fields {
		b.WriteString(f + "\n")
	}
	b.WriteString("}{}\n")
	b.WriteString("for _, tt := range tests {\n")
	b.WriteString("t.Run(tt.name, func(t *testing.T) {\n")
	b.WriteString(body + "\n")
	b.WriteString("})\n")
	b.WriteString("}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// addTestOp scaffolds a table-driven test for a target function.
//
// v1 addresses the generated test by name in follow-up ops
// (add_test_case/set_test_case/remove_test_case's "test" field) rather than
// binding a handle the way statement ops do; see docs/specs/language.md's
// Test ops table. Table-handle addressing is a possible future op, not v1's.
type addTestOp struct{}

func (addTestOp) name() string { return "add_test" }

func (addTestOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg    string `json:"pkg"`
		Target string `json:"target"`
		Name   string `json:"name"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	target := orDefault(a.Target, ctx.sym)
	if target == "" {
		return &Reject{Reason: "add_test requires a target"}
	}
	testName := a.Name
	if testName == "" {
		testName = "Test" + strings.ReplaceAll(target, ".", "_")
	}
	if !token.IsIdentifier(testName) {
		return &Reject{Reason: "test name is not a valid identifier", Detail: testName}
	}
	if _, _, rej := ctx.s.findObject(pkg, testName); rej == nil {
		return &Reject{Reason: "test already exists", Detail: pkg + "." + testName}
	}

	fn, testFile, insertOffset, needsCreate, pkgName, extPkg, testify, rej := addTestFileEdit(ctx.s, pkg, target)
	if rej != nil {
		return rej
	}
	funcText, rej := renderTestFunc(testName, target, fn, extPkg, pkgName, testify)
	if rej != nil {
		return rej
	}

	if !needsCreate {
		ctx.addBaseline(preflightBaseline(ctx.s, testFile, pkg))
		e := edit{testFile, insertOffset, 0, "\n" + funcText + "\n"}
		if rej := ctx.applyDeclEdits([]edit{e}); rej != nil {
			return rej
		}
		ctx.noteTouched(pkg, testName, false)
		ctx.postChecks = append(ctx.postChecks, func() *Reject {
			if _, _, rej := ctx.s.findObject(pkg, testName); rej != nil {
				return &Reject{Reason: "declaration missing after edit", Detail: testName}
			}
			return nil
		})
		return nil
	}

	// New file: like UpsertDecl's own needsCreate path, a new file changes
	// the package's CompiledGoFiles, which the incremental retypecheck path
	// never picks up — only a full reload does. Write and reload immediately
	// rather than through the ctx.src ledger.
	//
	// Cleanup on any failure below — this op's own, or a later op's in the
	// same patch, or a dry_run's unconditional restore — is entirely
	// patchComposable's job (ctx.cleanupFileOps) once the file is
	// registered in ctx.createdFiles just below: this op must not delete it
	// itself, or a later executor-level cleanup double-deletes (harmless for
	// os.Remove itself, but the accompanying forced reload would then run
	// twice for no reason). That single cleanup path is also what retires
	// the old v1 ceiling here ("compose a file-creating add_test with other
	// ops across separate patches") — composing is now safe.
	//
	// before captures the target package's (plus its reverse importers')
	// diagnostics as they stood before this file existed. A package that
	// already had unrelated rot will still show those same diagnostics after
	// the file is written and the workspace reloaded; filtering against them
	// keeps pre-existing rot from blocking the generated test (or being
	// blamed on it) — only NEW diagnostics reject. The same baseline joins
	// patchCtx so patchComposable's end-of-list retypecheck tolerates the
	// rot too.
	before := errorSet(errorsIn(ctx.s.affected(pkg)))
	ctx.addBaseline(before)
	clause := "package " + pkgName
	if extPkg {
		clause += "_test"
	}
	src := clause + "\n\n" + funcText + "\n"
	fixed, err := imports.Process(testFile, []byte(src), nil)
	if err != nil {
		return &Reject{Reason: "generated test does not parse", Detail: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(testFile), 0o755); err != nil {
		return &Reject{Reason: "failed to create test file", Detail: err.Error()}
	}
	if err := os.WriteFile(testFile, fixed, 0o644); err != nil {
		return &Reject{Reason: "failed to create test file", Detail: err.Error()}
	}
	ctx.createdFiles = append(ctx.createdFiles, testFile)
	ctx.s.loaded = false
	if _, err := ctx.s.load(); err != nil {
		return &Reject{Reason: "workspace failed to reload", Detail: err.Error()}
	}
	if diags := filterNew(errorsIn(ctx.s.affected(pkg)), before); len(diags) > 0 {
		return &Reject{Reason: "generated test does not typecheck", Diagnostics: diags}
	}
	if _, _, rej := ctx.s.findObject(pkg, testName); rej != nil {
		return &Reject{Reason: "declaration missing after edit", Detail: testName}
	}
	ctx.s.noteWrite(testFile)
	ctx.noteTouched(pkg, testName, false)
	return nil
}

// testTable locates a table-driven test generated by add_test: the target
// TestXxx function's declaration file and its "tests := []struct{...}{...}"
// case-row composite literal.
func testTable(s *Snapshot, pkgPath, test string) (testFile string, lit *ast.CompositeLit, rej *Reject) {
	p, obj, rej0 := s.findObject(pkgPath, test)
	if rej0 != nil {
		return "", nil, rej0
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return "", nil, &Reject{Reason: "symbol is not a function", Detail: objKind(obj)}
	}
	decl, file := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return "", nil, &Reject{Reason: "function declaration not found", Detail: test}
	}
	for _, stmt := range decl.Body.List {
		as, ok := stmt.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			continue
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != "tests" {
			continue
		}
		if cl, ok := as.Rhs[0].(*ast.CompositeLit); ok {
			return file, cl, nil
		}
	}
	return "", nil, &Reject{Reason: "test cases table not found", Detail: test + " was not generated by add_test"}
}

// tableFields returns the case struct's field names in declaration order
// (always led by "name", per add_test's own generated shape).
func tableFields(lit *ast.CompositeLit) ([]string, *Reject) {
	at, ok := lit.Type.(*ast.ArrayType)
	if !ok {
		return nil, &Reject{Reason: "test cases table has an unexpected shape"}
	}
	st, ok := at.Elt.(*ast.StructType)
	if !ok {
		return nil, &Reject{Reason: "test cases table has an unexpected shape"}
	}
	var names []string
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return nil, &Reject{Reason: "test cases table has an unexpected shape", Detail: "embedded field"}
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	if len(names) == 0 || names[0] != "name" {
		return nil, &Reject{Reason: "test cases table has an unexpected shape"}
	}
	return names, nil
}

// rowName extracts a case row's "name: ..." field value, for locating rows
// by name (set_test_case/remove_test_case) and for listing existing names
// (did_you_mean on a miss).
func rowName(row ast.Expr) (string, bool) {
	cl, ok := row.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "name" {
			continue
		}
		bl, ok := kv.Value.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			continue
		}
		v, err := strconv.Unquote(bl.Value)
		if err != nil {
			continue
		}
		return v, true
	}
	return "", false
}

func rowNames(lit *ast.CompositeLit) []string {
	var out []string
	for _, e := range lit.Elts {
		if n, ok := rowName(e); ok {
			out = append(out, n)
		}
	}
	return out
}

// findRow locates an existing case row by name, or "test case not found"
// with the existing row names as did_you_mean.
func findRow(lit *ast.CompositeLit, name string) (ast.Expr, *Reject) {
	for _, e := range lit.Elts {
		if n, ok := rowName(e); ok && n == name {
			return e, nil
		}
	}
	return nil, &Reject{Reason: "test case not found", Detail: name, DidYouMean: nearestCase(name, rowNames(lit))}
}

// nearestCase suggests existing row names close to a miss — same shape as
// nearestOps/nearestHandles for their own address spaces.
func nearestCase(miss string, names []string) []string {
	lower := strings.ToLower(miss)
	var hits []string
	for _, n := range names {
		ln := strings.ToLower(n)
		if ln == lower || strings.Contains(ln, lower) || strings.Contains(lower, ln) {
			hits = append(hits, n)
		}
	}
	if len(hits) == 0 {
		hits = append([]string(nil), names...)
	}
	if len(hits) > 3 {
		hits = hits[:3]
	}
	return hits
}

// buildRow renders one case row as a keyed composite literal:
// {name: "...", field: value, ...}. args fill the param-derived fields in
// order, want fills the want-derived fields (wantErr included) in order;
// values are expression atoms, syntax-validated the same way statement ops
// validate theirs (ctx.exprAtom) — type errors (a string want for an int
// field) surface at end-of-list typecheck, not here.
func buildRow(name string, fields, args, want []string, atom func(string) (string, *Reject)) (string, *Reject) {
	vals := append(append([]string{}, args...), want...)
	if len(vals) != len(fields) {
		return "", &Reject{Reason: "case value count does not match table fields",
			Detail: fmt.Sprintf("table fields: %s; got %d args + %d want", strings.Join(fields, ", "), len(args), len(want))}
	}
	var b strings.Builder
	b.WriteString("{name: ")
	b.WriteString(strconv.Quote(name))
	for i, f := range fields {
		v, rej := atom(vals[i])
		if rej != nil {
			return "", rej
		}
		b.WriteString(", ")
		b.WriteString(f)
		b.WriteString(": ")
		b.WriteString(v)
	}
	b.WriteString("}")
	return b.String(), nil
}

// addTestCaseOp appends one row to a table-driven test.
type addTestCaseOp struct{}

func (addTestCaseOp) name() string { return "add_test_case" }

func (addTestCaseOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string   `json:"pkg"`
		Test string   `json:"test"`
		Name string   `json:"name"`
		Args []string `json:"args"`
		Want []string `json:"want"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	test := orDefault(a.Test, ctx.sym)
	file, lit, rej := testTable(ctx.s, pkg, test)
	if rej != nil {
		return rej
	}
	fields, rej := tableFields(lit)
	if rej != nil {
		return rej
	}
	row, rej := buildRow(a.Name, fields[1:], a.Args, a.Want, ctx.exprAtom)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, file, pkg))
	// The trailing newline before Rbrace matters, not just cosmetics: it
	// puts the closing brace on its own line, so gofmt keeps (and a later
	// add_test_case's insertion can rely on) the row's trailing comma.
	// Without it, a single-line-then-brace row loses its comma on the next
	// gofmt pass, and a second appended row parses as "missing ',' before
	// newline in composite literal".
	offset := ctx.s.fset.Position(lit.Rbrace).Offset
	if rej := ctx.applyDeclEdits([]edit{{file, offset, 0, "\n" + row + ",\n"}}); rej != nil {
		return rej
	}
	ctx.noteTouched(pkg, test, false)
	return nil
}

// setTestCaseOp replaces an existing case row, addressed by its current name.
type setTestCaseOp struct{}

func (setTestCaseOp) name() string { return "set_test_case" }

func (setTestCaseOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string   `json:"pkg"`
		Test string   `json:"test"`
		Case string   `json:"case"`
		Name string   `json:"name"`
		Args []string `json:"args"`
		Want []string `json:"want"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	test := orDefault(a.Test, ctx.sym)
	file, lit, rej := testTable(ctx.s, pkg, test)
	if rej != nil {
		return rej
	}
	row, rej := findRow(lit, a.Case)
	if rej != nil {
		return rej
	}
	fields, rej := tableFields(lit)
	if rej != nil {
		return rej
	}
	newRow, rej := buildRow(orDefault(a.Name, a.Case), fields[1:], a.Args, a.Want, ctx.exprAtom)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, file, pkg))
	start := ctx.s.fset.Position(row.Pos()).Offset
	end := ctx.s.fset.Position(row.End()).Offset
	if rej := ctx.applyDeclEdits([]edit{{file, start, end - start, newRow}}); rej != nil {
		return rej
	}
	ctx.noteTouched(pkg, test, false)
	return nil
}

// removeTestCaseOp deletes an existing case row (its whole line, including
// the trailing comma), addressed by name.
type removeTestCaseOp struct{}

func (removeTestCaseOp) name() string { return "remove_test_case" }

func (removeTestCaseOp) apply(ctx *patchCtx, raw json.RawMessage) *Reject {
	var a struct {
		Pkg  string `json:"pkg"`
		Test string `json:"test"`
		Case string `json:"case"`
	}
	if rej := decodeOpArgs(raw, &a); rej != nil {
		return rej
	}
	pkg := orDefault(a.Pkg, ctx.pkg)
	test := orDefault(a.Test, ctx.sym)
	file, lit, rej := testTable(ctx.s, pkg, test)
	if rej != nil {
		return rej
	}
	row, rej := findRow(lit, a.Case)
	if rej != nil {
		return rej
	}
	ctx.addBaseline(preflightBaseline(ctx.s, file, pkg))
	src, err := os.ReadFile(file)
	if err != nil {
		return &Reject{Reason: "file not found", Detail: file}
	}
	start := lineStart(src, ctx.s.fset.Position(row.Pos()).Offset)
	end := lineEndNL(src, ctx.s.fset.Position(row.End()).Offset)
	if rej := ctx.applyDeclEdits([]edit{{file, start, end - start, ""}}); rej != nil {
		return rej
	}
	ctx.noteTouched(pkg, test, false)
	return nil
}

func init() {
	opRegistry["add_test"] = func() patchOp { return addTestOp{} }
	opRegistry["add_test_case"] = func() patchOp { return addTestCaseOp{} }
	opRegistry["set_test_case"] = func() patchOp { return setTestCaseOp{} }
	opRegistry["remove_test_case"] = func() patchOp { return removeTestCaseOp{} }
	declOps["add_test"] = true
	declOps["add_test_case"] = true
	declOps["set_test_case"] = true
	declOps["remove_test_case"] = true
}
