package bench

import (
	"strings"
	"testing"
)

func TestAppendParamToSig(t *testing.T) {
	cases := []struct {
		sig, name, typ, want string
	}{
		{"(ctx context.Context) error", "_", "time.Duration", "(ctx context.Context, _ time.Duration) error"},
		{"()", "x", "int", "(x int)"},
		{"(a, b int) (int, error)", "c", "bool", "(a, b int, c bool) (int, error)"},
		{"(a int, rest ...string) error", "x", "bool", "(a int, x bool, rest ...string) error"},
		{"(ctx context.Context, _ proxy.DecryptFn) (proxy.ProxyConnFn, error)", "controlCtx", "context.Context",
			"(ctx context.Context, _ proxy.DecryptFn, controlCtx context.Context) (proxy.ProxyConnFn, error)"},
	}
	for _, c := range cases {
		got, err := appendParamToSig(c.sig, c.name, c.typ)
		if err != nil {
			t.Errorf("appendParamToSig(%q): %v", c.sig, err)
			continue
		}
		if got != c.want {
			t.Errorf("appendParamToSig(%q) = %q, want %q", c.sig, got, c.want)
		}
	}
}

func TestRenameParamOfType(t *testing.T) {
	got, err := renameParamOfType("(_ context.Context) error", "context.Context", "ctx")
	if err != nil || got != "(ctx context.Context) error" {
		t.Errorf("got %q, %v", got, err)
	}
	// Already carrying the name: unchanged.
	got, err = renameParamOfType("(ctx context.Context) error", "context.Context", "ctx")
	if err != nil || got != "(ctx context.Context) error" {
		t.Errorf("got %q, %v", got, err)
	}
	// No param of that type: unchanged, not an error.
	got, err = renameParamOfType("(n int) error", "context.Context", "ctx")
	if err != nil || got != "(n int) error" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPickAddedParam(t *testing.T) {
	specs := []AddParamSpec{
		{Sym: "a.Run", Name: "_", Type: "time.Duration"},
		{Sym: "b.Run", Name: "statusThreshold", Type: "time.Duration"},
		{Sym: "c.Run", Name: "_", Type: "time.Duration"},
		{Sym: "d.Run", Name: "ctx", Type: "context.Context"},
	}
	name, typ := pickAddedParam(specs)
	if typ != "time.Duration" || name != "statusThreshold" {
		t.Errorf("got %s %s", name, typ)
	}
	// All underscores: the name stays underscore.
	name, typ = pickAddedParam(specs[:1])
	if typ != "time.Duration" || name != "_" {
		t.Errorf("got %s %s", name, typ)
	}
}

func TestPkgPathFor(t *testing.T) {
	got := pkgPathFor("/tmp/wt", "example.com/mod", "/tmp/wt/internal/census/census.go")
	if got != "example.com/mod/internal/census" {
		t.Errorf("got %q", got)
	}
	if got := pkgPathFor("/tmp/wt", "example.com/mod", "/tmp/wt/main.go"); got != "example.com/mod" {
		t.Errorf("root pkg: got %q", got)
	}
}

const composeFixture = `package fix

import (
	"context"
	aud "example.com/mod/audit"
)

// Factory builds a backend.
type Factory func(context.Context, *Config) (Backend, error)

type Config struct{}
type Backend interface{}

func New(f Factory, cfg *Config) (Backend, error) {
	return f(context.Background(), cfg)
}

func helperFactory() aud.Factory {
	return func(ctx context.Context, c *aud.Config) (aud.Backend, error) {
		return nil, nil
	}
}

func Register() {
	fn := func(context.Context, *Config) (Backend, error) {
		return nil, nil
	}
	use(fn)
}

func use(Factory) {}
`

func TestAppendTypeToFuncTypeDecl(t *testing.T) {
	got, err := appendTypeToFuncTypeDecl([]byte(composeFixture), "fix.go", "Factory", "bool")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "type Factory func(context.Context, *Config, bool) (Backend, error)") {
		t.Errorf("appended type text wrong:\n%s", got)
	}
	if !strings.Contains(got, "// Factory builds a backend.") {
		t.Errorf("doc comment lost:\n%s", got)
	}
}

func TestResolveTypeQualifier(t *testing.T) {
	// Unqualified: the diagnostic file's own package path.
	pkg, name, err := resolveTypeRef("Factory", []byte(composeFixture), "example.com/mod/fix")
	if err != nil || pkg != "example.com/mod/fix" || name != "Factory" {
		t.Errorf("got %q %q %v", pkg, name, err)
	}
	// Qualified: resolved through the file's imports, alias included.
	pkg, name, err = resolveTypeRef("aud.Factory", []byte(composeFixture), "example.com/mod/fix")
	if err != nil || pkg != "example.com/mod/audit" || name != "Factory" {
		t.Errorf("got %q %q %v", pkg, name, err)
	}
}

func TestDiagnosticRegexes(t *testing.T) {
	lit := `cannot use (func(_ context.Context, config *audit.BackendConfig) (audit.Backend, error) literal) (value of type func(_ "context".Context, config *audit.BackendConfig) (audit.Backend, error)) as audit.Factory value in return statement`
	if m := reLitAsValue.FindStringSubmatch(lit); m == nil || m[1] != "audit.Factory" {
		t.Errorf("typed literal form: %v", m)
	}
	if m := reLitAsValue.FindStringSubmatch(`cannot use func literal (value of type func(int)) as Handler value in assignment`); m == nil || m[1] != "Handler" {
		t.Errorf("short literal form: %v", m)
	}
	if reVarAsValue.MatchString(lit) || reFuncAsValue.MatchString(lit) {
		t.Errorf("literal message must not match the var or named-func rules")
	}
	fn := `cannot use auditFile.Factory (value of type func(ctx "context".Context, conf *audit.BackendConfig, useEventLogger bool) (audit.Backend, error)) as audit.Factory value in map literal`
	if m := reFuncAsValue.FindStringSubmatch(fn); m == nil || m[1] != "auditFile.Factory" || m[2] != "audit.Factory" {
		t.Errorf("named func form: %v", m)
	}
	v := `cannot use fn (variable of type func("context".Context, DecryptFn) (ProxyConnFn, error)) as Handler value in argument to RegisterHandler`
	if m := reVarAsValue.FindStringSubmatch(v); m == nil || m[1] != "fn" || m[2] != "Handler" {
		t.Errorf("var form: %v", m)
	}
	c := "not enough arguments in call to f\n\thave (\"context\".Context)\n\twant (\"context\".Context, bool)"
	if m := reNotEnoughArgs.FindStringSubmatch(c); m == nil || m[1] != "f" {
		t.Errorf("call form: %v", m)
	}
}

func TestDeclRepairsBuildBodies(t *testing.T) {
	rs := newRepairSet("/x/wt", "example.com/mod")
	src := []byte(composeFixture)
	// Append an argument to the call inside New.
	if err := rs.appendArgToCalls(src, "fix.go", "example.com/mod/fix", "f", "false"); err != nil {
		t.Fatal(err)
	}
	// Append a bare param type to the literal assigned to fn inside Register.
	if err := rs.appendParamToVarLiterals(src, "fix.go", "example.com/mod/fix", "fn", "bool"); err != nil {
		t.Fatal(err)
	}
	ops := rs.ops()
	if len(ops) != 2 {
		t.Fatalf("want 2 set_body ops, got %v", ops)
	}
	bodies := map[string]string{}
	for _, op := range ops {
		if op["op"] != "set_body" {
			t.Fatalf("want set_body, got %v", op)
		}
		bodies[op["sym"].(string)] = op["body"].(string)
	}
	if !strings.Contains(bodies["New"], "f(context.Background(), cfg, false)") {
		t.Errorf("call arg not appended:\n%s", bodies["New"])
	}
	if !strings.Contains(bodies["Register"], "func(context.Context, *Config, bool) (Backend, error)") {
		t.Errorf("literal param not appended:\n%s", bodies["Register"])
	}
	if strings.HasPrefix(strings.TrimSpace(bodies["New"]), "{") {
		t.Errorf("body must not carry braces:\n%s", bodies["New"])
	}
}

func TestRepairSetLiteralAtPosition(t *testing.T) {
	rs := newRepairSet("/x/wt", "example.com/mod")
	src := []byte(composeFixture)
	// The literal returned by helperFactory: repair it by line, the param
	// list is named so the appended param reads `_ bool`.
	line := 1 + strings.Count(composeFixture[:strings.Index(composeFixture, "return func(ctx")], "\n")
	if err := rs.appendParamToLiteralAt(src, "fix.go", "example.com/mod/fix", line, "bool"); err != nil {
		t.Fatal(err)
	}
	ops := rs.ops()
	if len(ops) != 1 {
		t.Fatalf("want 1 op, got %v", ops)
	}
	body := ops[0]["body"].(string)
	if !strings.Contains(body, "func(ctx context.Context, c *aud.Config, _ bool) (aud.Backend, error)") {
		t.Errorf("named literal must gain `_ bool`:\n%s", body)
	}
}
