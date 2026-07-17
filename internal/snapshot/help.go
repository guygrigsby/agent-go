package snapshot

import "encoding/json"

// catalogVersion identifies the shape of the catalog Help returns. Bump it
// whenever an op is added, removed, or its argument shape changes, so a
// caller can detect a stale cached copy instead of guessing from the op
// list's length.
const catalogVersion = "v6"

// helpArg documents one op argument's wire shape.
type helpArg struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// helpOp documents one patch op: its wire name, its arguments, one worked
// example formatted exactly as a patch envelope's "ops" array would carry
// it (so a caller can lift it verbatim), and any v1 ceiling worth flagging
// up front rather than discovering via a rejection. An example carries one
// to three ops, the last being the documented op — earlier ops set up state
// it needs (a switch for add_case, a scaffolded test for add_test_case).
// Every example is validated end to end against the demo fixture by
// TestHelpExamplesAcceptedByFixture (help_test.go).
type helpOp struct {
	Op      string          `json:"op"`
	Args    []helpArg       `json:"args"`
	Example json.RawMessage `json:"example"`
	Notes   string          `json:"notes,omitempty"`

	// execCeiling, when non-empty, names why this op's example cannot be
	// executed hermetically against the demo fixture (module ops that shell
	// out to go get need the network or a primed module cache, and any
	// pinned version string would rot). TestHelpExamplesAcceptedByFixture
	// skips execution for these — structural validation still applies —
	// so the exemption is explicit and enumerable, never silent.
	execCeiling string `json:"-"`
}

// helpTool documents one of the six MCP/daemon entry points (the four sugar
// ops — rename, set_body, add_param, upsert_decl — are each exactly a
// one-op patch and are documented as ops below, not repeated here).
type helpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// toolCatalog covers the six primary tools, in spec order (docs/specs/
// language.md "Surface"). query's kinds and patch's op families are
// summarized here; the full per-op schema lives in opCatalog.
var toolCatalog = []helpTool{
	{"status", "Load or refresh the workspace snapshot. Returns package and file counts and any type errors. No arguments."},
	{"help", "Return this versioned op catalog: every patch op's argument schema, one worked example, and its v1 ceilings, plus short descriptions of the six tools. No arguments."},
	{"query", "Semantic questions against the typechecked snapshot, dispatched by kind: search (case-insensitive name fragment -> exact addresses), inspect (kind, signature, decl position, doc), refs (every reference, tests included, defs marked), callers/callees (static call-graph edges; a call through an interface reports the interface method), implementations (interface -> implementing types, or type -> satisfied interfaces), doc (doc comment text). Args: kind (required), pkg, sym, q (name fragment, for kind=search, falls back to sym), offset (page offset for list results). Lists are position-sorted and paged 50 at a time: count is the total found; a truncated response carries truncated=true and next_offset to pass back as offset."},
	{"view", "Render a declaration as annotated text. Functions and methods get a per-statement nK: handle prefix plus a generation counter for staleness checks; other declarations (const, var, type) render as plain source. Handles are meaningful only against the generation the same response reports. Args: pkg, sym."},
	{"patch", "Apply an ordered list of ops as one atomic, generation-checked transaction: every op applies to an in-memory copy, the dirty set re-typechecks once, then everything writes and splices together — or nothing does. Ops compose: an op later in the list can address a handle an earlier op returned, referenced as $1, $2, ... by 1-based op index. dry_run runs the identical pipeline and reports accept/reject without writing. Op families (full schemas and examples via help): decl ops (rename, set_body, add_param, upsert_decl, delete_decl, set_doc, add_field, remove_field), statement ops (add_assign, add_call, add_return, add_if, add_for, add_switch, add_case, add_defer, add_go, set_cond, replace_expr, delete_node, wrap_stmts, wrap_error), test ops (add_test, add_test_case, set_test_case, remove_test_case), project ops (delete_file, move_file, add_dependency, remove_dependency, mod_tidy). A decl, test, or project op (rename, set_body, add_param, upsert_decl, delete_decl, set_doc, add_field, remove_field, add_test, add_test_case, set_test_case, remove_test_case) and a statement op cannot edit the same file in one patch; run them as separate patches. An accepted patch that touched exactly one declaration embeds that declaration's fresh view (same {text, nodes, generation} payload the view tool returns) under \"view\", so back-to-back edits need no view call in between; when several declarations were touched the response carries views_omitted instead. Args: pkg/sym (defaults for ops that omit them), generation, dry_run, ops (required, the array of op objects)."},
	{"test", "Run `go test`, scoped to a package (default the whole workspace) and optionally filtered by name, and return structured per-test results: pass/fail, elapsed time, and captured output for failures. Validation of mutations stays compiler-only; this is how you close the behavior loop after a set of changes. Args: pkg, run (a -run filter)."},
}

// opCatalog documents every op in opRegistry — decl, statement, and test
// families — one entry each, kept honest by TestHelpCatalogMatchesOpRegistry
// (help_test.go): every opRegistry key must appear here and vice versa.
var opCatalog = []helpOp{
	// Decl ops (docs/specs/language.md "Decl ops"). pkg/sym default to the
	// patch envelope's own pkg/sym when omitted, so a single-target patch
	// need not repeat them on every op.
	{
		Op: "rename",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
			{"to", "string", true, "new name"},
		},
		Example: json.RawMessage(`[{"op":"rename","sym":"Double","to":"Twice"}]`),
		Notes:   "proves post-splice resolution: every rewritten reference must resolve to the renamed object; reference capture rejects even when the compiler is satisfied",
	},
	{
		Op: "set_body",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
			{"body", "string", true, "new body as statements, no surrounding braces"},
		},
		Example: json.RawMessage(`[{"op":"set_body","sym":"Double","body":"return v + v"}]`),
		Notes:   "the coarse escape hatch: replaces the whole block between braces, validated by typecheck like every other op",
	},
	{
		Op: "add_param",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
			{"name", "string", true, "new parameter name"},
			{"type", "string", true, "new parameter type, e.g. context.Context"},
			{"default", "string", false, "argument expression for existing call sites; required whenever the function already has callers"},
		},
		Example: json.RawMessage(`[{"op":"add_param","pkg":"demo/sig","sym":"Scale","name":"offset","type":"int","default":"0"}]`),
		Notes:   "callers updated with default; a top-level local `name := <default>` in the body is superseded and deleted (parameters share the body scope), any other same-named body declaration is rejected with its position; references to the function as a value (assigned, passed, satisfying an interface) cannot be repaired and are rejected with their positions",
	},
	{
		Op: "upsert_decl",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"text", "string", true, "complete declaration source, including doc comment if any; the symbol name is parsed from it"},
		},
		Example: json.RawMessage(`[{"op":"upsert_decl","pkg":"demo/lib","text":"// Double doubles v.\nfunc Double(v int) int {\n\treturn v + v\n}"}]`),
		Notes:   "add or replace a whole top-level declaration; goimports runs in the loop. New declarations land in agent.go, created on demand — including a brand-new file or package mid-patch, so one atomic patch can create a package and move declarations into it",
	},
	{
		Op: "delete_decl",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
		},
		Example: json.RawMessage(`[{"op":"delete_decl","sym":"Unused"}]`),
		Notes:   "rejected while any non-declaring reference remains outside the declaration itself (a recursive self-call does not count); the diagnostics list where",
	},
	{
		Op: "set_doc",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
			{"text", "string", true, "doc comment body; each line is rendered with a \"// \" prefix"},
		},
		Example: json.RawMessage(`[{"op":"set_doc","sym":"Double","text":"Double doubles v."}]`),
		Notes:   "doc comment only; replaces an existing one rather than appending, and does not affect the typecheck surface",
	},
	{
		Op: "add_field",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "the struct type's name; defaults to the envelope's sym"},
			{"name", "string", true, "new field name"},
			{"type", "string", true, "new field type"},
			{"tag", "string", false, "struct tag, without backticks"},
		},
		Example: json.RawMessage(`[{"op":"add_field","sym":"Store","name":"Tag","type":"string"}]`),
		Notes:   "appended to the struct's field list; rejected if the name already exists",
	},
	{
		Op: "remove_field",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "field: Type.Field; defaults to the envelope's sym"},
		},
		Example: json.RawMessage(`[{"op":"remove_field","sym":"Config.Legacy"}]`),
		Notes:   "rejected while referenced. v1 ceiling: a field sharing a multi-name declaration (\"a, b int\") or an embedded field is not supported",
	},
	{
		Op: "move_decl",
		Args: []helpArg{
			{"pkg", "string", false, "source package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "top-level declaration name; defaults to the envelope's sym"},
			{"to_pkg", "string", true, "target package import path (must exist by the time this op runs; an earlier upsert_decl in the same patch can create it)"},
			{"create_pkg", "boolean", false, "create a missing module-local target package as part of this patch; without it a missing target rejects (typo safety) and offers this flag as a repair"},
		},
		Example: json.RawMessage(`[{"op":"move_decl","pkg":"demo/sig","sym":"Fetch","to_pkg":"demo/lib"}]`),
		Notes:   "relocates the whole declaration (doc comment included) and requalifies every reference, adding imports where needed; a type moves together with its whole method set, and one spec of a grouped const/var/type block extracts standalone. v1 ceilings: the declaration must be self-contained (no uses of its old package's other top-level symbols) and grouped specs may not lean on iota or an inherited value; each rejects with the blocking names",
	},
	{
		Op: "set_signature",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"sym", "string", false, "symbol: Name or Type.Member; defaults to the envelope's sym"},
			{"signature", "string", true, "the complete new signature as Go text: \"(params) results\""},
			{"defaults", "object", false, "argument expression per NEW parameter name, spliced into every existing call site; required for any new parameter when call sites exist"},
		},
		Example: json.RawMessage(`[{"op":"set_signature","pkg":"demo/sig","sym":"Fetch","signature":"(ctx context.Context, a int, b string, rest ...int) int","defaults":{"ctx":"context.Background()"}}]`),
		Notes:   "full parameter/result rewrite: parameters are matched to the old signature by name — carried ones keep each call site's argument (reordering reorders them), dropped ones drop it, new ones take their default; underscore params pair positionally when their type matches, so widening func(ctx context.Context, _ DecryptFn) keeps the _ argument; a spread call site f(args...) survives insertions before the variadic. Value uses of the function and the body itself are not rewritten: repair them with sibling ops in the same patch or the end-of-list typecheck rejects with the site positions",
	},

	// Project ops (docs/specs/language.md "Project ops"): the file tier.
	{
		Op: "delete_file",
		Args: []helpArg{
			{"path", "string", true, "file path relative to the module root (or absolute)"},
		},
		Example: json.RawMessage(`[{"op":"upsert_decl","pkg":"demo/lib","text":"func Tmp() int {\n\treturn 0\n}"},{"op":"delete_file","path":"lib/agent.go"}]`),
		Notes:   "removes one file; rejected while any package-level symbol it declares is referenced from outside it (the rejection lists the reference positions). Deleting a package's last file removes the package",
	},
	{
		Op: "move_file",
		Args: []helpArg{
			{"from", "string", true, "current file path relative to the module root (or absolute)"},
			{"to", "string", true, "new file path; a different directory must hold an already-loaded package"},
		},
		Example: json.RawMessage(`[{"op":"move_file","from":"lib/lib.go","to":"lib/core.go"}]`),
		Notes:   "same-directory moves are pure renames; a cross-package move rewrites the package clause to the target package and drops a now-self import, and is rejected while the file declares symbols referenced from outside it (their qualifiers would all be wrong — use move_decl per declaration instead)",
	},
	{
		Op: "add_dependency",
		Args: []helpArg{
			{"module", "string", true, "module path, e.g. golang.org/x/sync"},
			{"version", "string", false, "module version; defaults to latest"},
		},
		Example:     json.RawMessage(`[{"op":"add_dependency","module":"golang.org/x/sync","version":"v0.10.0"}]`),
		Notes:       "runs go get module@version against the workspace module; go.mod and go.sum restore byte-for-byte on any later rejection in the same patch. Needs the module in the local cache or network access",
		execCeiling: "go get needs the network or a primed module cache, and a pinned version rots",
	},
	{
		Op: "remove_dependency",
		Args: []helpArg{
			{"module", "string", true, "module path to drop"},
		},
		Example:     json.RawMessage(`[{"op":"remove_dependency","module":"golang.org/x/sync"}]`),
		Notes:       "runs go get module@none; rejected while any workspace file still imports the module (the rejection lists the import positions)",
		execCeiling: "dropping a requirement the fixture never had exercises nothing; the real path is covered by unit tests",
	},
	{
		Op:      "mod_tidy",
		Args:    []helpArg{},
		Example: json.RawMessage(`[{"op":"mod_tidy"}]`),
		Notes:   "runs go mod tidy with the same go.mod/go.sum restore-and-validate wrapper as the other module ops",
	},

	// Statement ops (docs/specs/language.md "Statement ops"). All address a
	// handle from the most recent view (or a $N reference to an earlier op
	// in the same patch) and take at/where for placement; there is no
	// pkg/sym here — statement ops work inside the envelope's own fixed
	// pkg/sym target.
	{
		Op: "add_assign",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last (first/last need a block-owning handle)"},
			{"lhs", "string", true, "assignment target identifier"},
			{"rhs", "string", true, "right-hand-side expression, parsed and typechecked in scope"},
			{"define", "bool", false, "use := instead of ="},
		},
		Example: json.RawMessage(`[{"op":"add_assign","at":"n2","where":"after","lhs":"_","rhs":"h(3)","define":false}]`),
	},
	{
		Op: "add_call",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"expr", "string", true, "a call expression or channel receive (<-ch); assignments belong to add_assign"},
		},
		Example: json.RawMessage(`[{"op":"add_call","at":"n2","where":"after","expr":"fmt.Println(h(1))"}]`),
	},
	{
		Op: "add_return",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"exprs", "[]string", false, "result expressions, in order; omit for a bare \"return\""},
		},
		Example: json.RawMessage(`[{"op":"add_return","at":"n2","where":"after","exprs":["h(2)"]}]`),
		Notes:   "arity and result types are checked against the enclosing signature at end-of-list typecheck, not here",
	},
	{
		Op: "add_defer",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"expr", "string", true, "a call expression"},
		},
		Example: json.RawMessage(`[{"op":"add_defer","at":"n1","where":"after","expr":"fmt.Println(\"done\")"}]`),
	},
	{
		Op: "add_go",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"expr", "string", true, "a call expression"},
		},
		Example: json.RawMessage(`[{"op":"add_go","at":"n2","where":"after","expr":"h(1)"}]`),
	},
	{
		Op: "delete_node",
		Args: []helpArg{
			{"at", "string", true, "handle of the statement or case clause to remove"},
		},
		Example: json.RawMessage(`[{"op":"add_return","at":"n3","where":"after","exprs":["h(9)"]},{"op":"delete_node","at":"n3"}]`),
		Notes:   "a block-owning statement with children, or an if with an else, is rejected rather than silently discarding content — delete children first",
	},
	{
		Op: "add_if",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"cond", "string", true, "condition expression"},
			{"else", "bool", false, "also create an empty else block"},
		},
		Example: json.RawMessage(`[{"op":"add_if","at":"n2","where":"after","cond":"h != nil","else":false}]`),
		Notes:   "returns the new then-block's own handle via $N; there is no v1 handle for a requested else block (view again to reach it)",
	},
	{
		Op: "add_for",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"cond", "string", false, "condition expression; mutually exclusive with range"},
			{"range", "string", false, "a full range clause, e.g. \"k, v := range coll\"; mutually exclusive with cond"},
		},
		Example: json.RawMessage(`[{"op":"add_for","at":"n2","where":"after","cond":"h(0) > 0"}]`),
		Notes:   "empty body, returns its handle via $N. v1 ceiling: no init/post clauses — use upsert_decl/set_body for a classic three-clause loop",
	},
	{
		Op: "add_switch",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) to place the statement relative to"},
			{"where", "string", true, "before | after | first | last"},
			{"tag", "string", false, "switch tag expression; omit for a tagless switch"},
		},
		Example: json.RawMessage(`[{"op":"add_switch","at":"n2","where":"after","tag":"h(1)"}]`),
		Notes:   "empty body; extend with add_case",
	},
	{
		Op: "add_case",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) of the switch statement to extend"},
			{"exprs", "[]string", false, "case expressions; mutually exclusive with default"},
			{"default", "bool", false, "make this the default clause; mutually exclusive with exprs"},
		},
		Example: json.RawMessage(`[{"op":"add_switch","at":"n2","where":"after","tag":"h(1)"},{"op":"add_case","at":"$1","exprs":["1","2"]}]`),
		Notes:   "always appends as the last clause (v1 has no argument for placing a case among existing ones); returns the new case's body handle via $N",
	},
	{
		Op: "set_cond",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) of the if/for/case to retarget"},
			{"expr", "string", true, "replacement condition"},
		},
		Example: json.RawMessage(`[{"op":"add_if","at":"n2","where":"after","cond":"h == nil"},{"op":"set_cond","at":"$1","expr":"h != nil"}]`),
		Notes:   "a case clause's whole expression list is replaced as one; v1 has no per-element case-expr addressing",
	},
	{
		Op: "replace_expr",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) of the target node"},
			{"expr", "string", true, "replacement expression"},
		},
		Example: json.RawMessage(`[{"op":"add_call","at":"n2","where":"after","expr":"h(1)"},{"op":"replace_expr","at":"$1","expr":"h(2)"}]`),
		Notes:   "v1 ceiling: an if/for/case condition or a whole expression statement only; per-argument sub-expression handles are future work",
	},
	{
		Op: "wrap_stmts",
		Args: []helpArg{
			{"from", "string", true, "handle (or $N) of the first statement to enclose"},
			{"to", "string", true, "handle (or $N) of the last statement to enclose"},
			{"with", "string", true, "if | for | block"},
			{"cond", "string", false, "condition; required for with=if/for, forbidden for with=block"},
		},
		Example: json.RawMessage(`[{"op":"wrap_stmts","from":"n1","to":"n2","with":"if","cond":"helper(1) > 0"}]`),
		Notes:   "from/to must be direct siblings, in order, of the same statement list; returns the new node's handle via $N",
	},
	{
		Op: "wrap_error",
		Args: []helpArg{
			{"at", "string", true, "handle (or $N) of the assignment or expression-statement call to wrap"},
			{"message", "string", true, "context prefix for fmt.Errorf(\"...: %w\", err)"},
		},
		Example: json.RawMessage(`[{"op":"wrap_error","at":"n1","message":"fetch"}]`),
		Notes:   "the Go idiom automated end to end: binds err, inserts \"if err != nil { return ..., fmt.Errorf(...) }\". v1 ceiling: a bare expression-statement call resolves its return arity only for a same-package function identifier",
	},

	// Test ops (docs/specs/language.md "Test ops"). Decl-shaped (pkg/target
	// or pkg/test default from the envelope), scaffolding the idiomatic
	// table-driven form rather than synthesizing it via upsert_decl.
	{
		Op: "add_test",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"target", "string", true, "the function under test; defaults to the envelope's sym when omitted"},
			{"name", "string", false, "test function name; defaults to Test<Target>"},
		},
		Example: json.RawMessage(`[{"op":"add_test","target":"Double"}]`),
		Notes:   "scaffolds a table-driven test: case struct derived from the target's signature, rows slice, range+t.Run loop. v1 targets a plain function, not a method; address the generated test by name in follow-up ops",
	},
	{
		Op: "add_test_case",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"test", "string", false, "test function name (from add_test); defaults to the envelope's sym"},
			{"name", "string", true, "case row name"},
			{"args", "[]string", false, "argument expressions, in target parameter order"},
			{"want", "[]string", false, "expected-result expressions, in result order (wantErr last, if the target returns error)"},
		},
		Example: json.RawMessage(`[{"op":"add_test","target":"Double"},{"op":"add_test_case","test":"TestDouble","name":"positive","args":["2"],"want":["4"]}]`),
		Notes:   "values are expression atoms, typechecked against the case struct at end-of-list typecheck",
	},
	{
		Op: "set_test_case",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"test", "string", false, "test function name; defaults to the envelope's sym"},
			{"case", "string", true, "existing row's current name"},
			{"name", "string", false, "new row name; defaults to case (no rename)"},
			{"args", "[]string", false, "replacement argument expressions"},
			{"want", "[]string", false, "replacement expected-result expressions"},
		},
		Example: json.RawMessage(`[{"op":"set_test_case","test":"TestScale","case":"one","args":["3","3"],"want":["9"]}]`),
	},
	{
		Op: "remove_test_case",
		Args: []helpArg{
			{"pkg", "string", false, "package import path; defaults to the envelope's pkg"},
			{"test", "string", false, "test function name; defaults to the envelope's sym"},
			{"case", "string", true, "row name to remove"},
		},
		Example: json.RawMessage(`[{"op":"remove_test_case","test":"TestScale","case":"one"}]`),
	},
}

// Help returns the versioned op catalog and tool summaries. It is static
// data derived from opCatalog/toolCatalog, not a query against the loaded
// workspace: no lock, no ensureFresh, safe to call from a fresh, unloaded
// Snapshot.
func (s *Snapshot) Help() (map[string]any, error) {
	return map[string]any{
		"status":  "ok",
		"version": catalogVersion,
		"tools":   toolCatalog,
		"ops":     opCatalog,
	}, nil
}
