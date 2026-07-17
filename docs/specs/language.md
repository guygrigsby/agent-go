# The ago language

The complete semantic edit language for Go agents. Supersedes the op
catalog in protocol.md (which documents the currently implemented surface);
engine semantics (daemon, snapshot, splice) are unchanged and specified
there and in the ADRs.

Design stance, fixed by the bench evidence and Guy's calls (2026-07-15):
statement-granular ops so the model composes operations, not syntax;
expression arguments are text atoms now with a structured form designed in;
node handles from views address everything inside a declaration; a patch is
an ordered, atomic, generation-checked transaction. Be as structured as
possible; start looser to get going.

## Surface

Six tools, four sugar ops. Everything else is patch payload.

| tool | purpose |
|---|---|
| `status` | load/refresh; packages, files, errors |
| `help` | versioned op catalog with per-op schemas and examples |
| `query` | semantic questions: `search, inspect, refs, callers, callees, implementations, doc` |
| `view` | render a declaration with node handles and its generation |
| `patch` | ordered op list, validated and applied atomically; `dry_run` for preview |
| `rename`, `set_body`, `add_param`, `upsert_decl` | standalone sugar, each exactly a one-op patch |

## Addressing

- Packages: import path. Symbols: `Name` or `Type.Member` (methods,
  fields). Test-file symbols resolve through test variants.
- Inside a declaration: node handles (`n1`, `n7`, ...) issued by `view`.
  A handle is meaningful only against the generation the view reported.
- Every declaration has a generation, bumped by any accepted mutation that
  touches it. A patch names the generation it was built against; a
  mismatch rejects with `stale generation: re-view`, never a guess.

Locals and import aliases are addressable only through handles (their
declaring node), never by name; that closes the addressing gap without a
second naming scheme.

## Queries

All from the typechecked snapshot (go/types tier); milliseconds.

- `search {q}`: case-insensitive name fragment to exact addresses.
- `inspect {pkg, sym}`: kind, signature, decl position, doc.
- `refs {pkg, sym}`: every reference, tests included, defs marked.
- `callers {pkg, sym}` / `callees {pkg, sym}`: static call-graph edges
  from types info. A call through an interface reports the interface
  method (query the interface method for its callers; `implementations`
  bridges to concrete types). v1 ceiling: no method-set candidate
  expansion for dynamic dispatch.
- `implementations {pkg, sym}`: interface -> implementing types, or
  type -> interfaces satisfied.
- `doc {pkg, sym}`: doc comment.

## View

`view {pkg, sym}` returns the declaration as annotated text, one handle
per statement and per addressable expression slot, plus `generation`:

```
gen 14
func (s *Store) Put(v int) error {
  n1: if s.frozen {
  n2:   return ErrFrozen
      }
  n3: s.n = v
  n4: return nil
}
```

Views are projections; agents read them, never edit them.

## Patch

```json
{"pkg": "demo/lib", "sym": "UseHelper", "generation": 14, "dry_run": false,
 "ops": [{"op": "add_if", "at": "n1", "where": "after", "cond": "v > 0"},
         {"op": "add_return", "at": "$1", "where": "first", "exprs": ["v"]}]}
```

Ordered. Validated as one unit: all ops apply to an in-memory copy, the
dirty set re-typechecks once, resolution proofs run once, then everything
writes and splices, or nothing does. Ops later in the list may address
handles created by earlier ops (constructors return handles in the
response; in one patch they are referenced as `$1`, `$2`, ... by op index).
`dry_run` runs the whole pipeline and reports the outcome without writing.

Cross-declaration patches (e.g. interface + impls rename) name pkg/sym per
op instead of at the top level.

### Decl ops

| op | args | notes |
|---|---|---|
| `upsert_decl` | pkg, text | add or replace a whole declaration; goimports in the loop; creates packages under the module on demand |
| `delete_decl` | pkg, sym | rejected while references remain (listed) |
| `move_decl` | pkg, sym, to_pkg, create_pkg? | rewrites references and imports, requalifying call sites; the declaration's own imports travel with it, aliases included; test decls land in a `_test.go`, created on demand. `create_pkg` creates a missing module-local target (opt in; a bare miss rejects and offers the flag as a repair). a type moves with its whole method set; one spec of a grouped block extracts standalone. v1 ceilings: the declaration must be self-contained (no uses of its old package's other top-level symbols); grouped specs may not lean on iota or inherited values; each rejects naming the blocker |
| `rename` | pkg, sym, to | proves post-splice resolution; rejects capture and collision |
| `set_body` | pkg, sym, body | body as checked text; the coarse escape hatch |
| `set_signature` | pkg, sym, signature, defaults? | full param/result rewrite as Go text. Parameters match the old signature by name: carried ones keep each call site's argument (reordering reorders them), dropped ones drop it, new ones splice their `defaults` entry positionally, so spread sites `f(args...)` survive insertions before the variadic. Underscore params pair positionally when their type matches, so widening `func(ctx context.Context, _ DecryptFn)` carries the `_` argument. Interface methods work; changing an interface and its implementors is one atomic multi-op patch. Value uses and the body are NOT rewritten: repair them with sibling ops in the same patch or the end-of-list typecheck rejects with the positions |
| `add_param` | pkg, sym, name, type, default | callers updated with default, inserted before a variadic tail (spread sites included); a top-level body local `name := <default>` is promoted into the parameter, any other same-named local rejects with its position; value uses rejected with theirs |
| `remove_param` | pkg, sym, name | UNSHIPPED (use set_signature, which drops params today); planned as sugar over it |
| `add_field` | pkg, sym (Type), name, type, tag? | |
| `remove_field` | pkg, sym (Type.Field) | rejected while references remain |
| `set_doc` | pkg, sym, text | doc comment only |
| `implement_interface` | pkg, type, iface | UNSHIPPED; generates missing method stubs |

### Statement ops

All take `{at: handle, where: before|after|first|last}` for placement
(`first`/`last` against a block handle). Expression-valued arguments are
text atoms, parsed and typechecked in scope at the target position.

| op | args | notes |
|---|---|---|
| `add_assign` | lhs, rhs, define? | `:=` when define |
| `add_return` | exprs[] | arity/type checked against signature |
| `add_call` | expr | expression statement |
| `add_if` | cond, else? | creates empty block(s); returns the then-block handle (else block addressed via a fresh view) |
| `add_for` | cond? or range? | empty body; returns handle. v1 ceiling: no init/post clauses; use upsert_decl/set_body for classic three-clause loops |
| `add_switch` | tag? | empty; extend with add_case |
| `add_case` | at (switch handle), exprs[] or default | returns body handle |
| `add_defer` | expr | |
| `add_go` | expr | |
| `set_cond` | at, expr | if/for/case condition replacement |
| `replace_expr` | at, expr | v1 ceiling: the node's condition or a whole expression statement only; per-slot sub-expression handles are future |
| `delete_node` | at | statement or case; deleting a block requires it be empty |
| `wrap_stmts` | from, to, with (if/for/block), cond? | from/to must be siblings in order in the same block; returns the new node's handle |
| `wrap_error` | at (assign or call handle), message | the Go idiom: assign err, add `if err != nil` return with `fmt.Errorf("...: %w", err)`. v1 ceiling: a bare expression-statement call resolves its return arity only for a same-package function identifier |

The statement vocabulary deliberately omits constructs an agent should
express with `upsert_decl`/`set_body` wholesale (select, labeled
statements, complex composite literals); `help` says so per gap. If bench
evidence shows a missing op mattering, it gets added; the catalog is
versioned.

### Test ops

Tests are declarations underneath, but they get dedicated ops for three
reasons: placement is constrained (a `_test.go` file, correct test
package), the naming is constrained (`TestXxx(t *testing.T)`), and the
idiomatic form humans expect, table-driven, is structured enough that an
agent should compose it from data, not synthesize its shape.

| op | args | notes |
|---|---|---|
| `add_test` | pkg, target (sym under test), name? | scaffolds a table-driven test: case struct derived from the target's signature (inputs from params, `want` from results), rows slice, `range` + `t.Run` loop, one starter failure message. v1: address the test by name in follow-up ops. Name defaults to `Test<Target>` |
| `add_test_case` | test (name), name, args[], want[] | appends one row; values are expression atoms typechecked against the case struct. v1 addresses tests by name; table handles are future |
| `set_test_case` / `remove_test_case` | case addressed by test + row name | |
| `add_bench` | pkg, target, name? | UNSHIPPED; `BenchmarkXxx(b *testing.B)` skeleton |

Placement and form rules, enforced at validation:

- New tests land in `<declfile>_test.go` next to the target, created on
  demand. Internal vs external test package follows the package's
  existing tests; a package with no tests gets internal.
- Assertion style follows the package's dominant existing convention
  (stdlib `t.Errorf` vs testify `require`/`assert`), detected from the
  test files already present; stdlib when there is no precedent (v1
  detection: any existing `_test.go` importing testify/require flips to
  `require.Equal`; `assert` unsupported).
- The canonical skeleton is fixed by this spec (name/args/want struct,
  `got` := call, comparison, `t.Errorf("Target(%v) = %v, want %v", ...)`)
  so generated tests read the same everywhere; gofmt applies as always.
- Generated helpers call `t.Helper()`.

Arbitrary non-table tests remain expressible with `upsert_decl` into a
`_test.go` path; the ops cover the idiomatic 90%.

### The test tool

Semantic mode has no shell, so running tests is part of the language: a
`test {pkg?, run?}` tool executes `go test` scoped to a package (and
optionally `-run` filter) and returns structured results: pass/fail per
test, failure messages with positions, elapsed time. Per Guy's workflow
rule, validation of mutations stays compiler-only; `test` is how the agent
closes the behavior loop per set of changes, at its own judgment. The
bench's scoring runs tests independently either way.

### Project ops

| op | args | notes |
|---|---|---|
| `add_dependency` | module, version? | go get + tidy, validated build |
| `remove_dependency` | module | rejected while imported |
| `move_file` | from, to | package clauses and imports updated |
| `delete_file` | path | rejected while it declares referenced symbols |
| `mod_tidy` | | |

## Expressions: atoms now, structure designed in

Every expression-typed argument accepts either a string (text atom) or a
structured node. The structured grammar is fixed now so it can arrive
per-argument without a protocol break:

```json
{"kind": "binary", "op": "!=", "left": {"kind": "ident", "name": "err"},
 "right": {"kind": "ident", "name": "nil"}}
```

Kinds: `ident, select, index, call, lit, unary, binary, paren, func`
(closure bodies are op lists). v1 implements text atoms; the structured
form is the target for constrained decoding (one JSON grammar covers a
whole patch, so a llama.cpp GBNF grammar can force validity at the
decoder).

## Rejections

```json
{"status": "rejected", "reason": "...", "detail": "...",
 "diagnostics": [{"pos": "...", "msg": "..."}],
 "did_you_mean": ["..."],
 "possible_repairs": [{"why": "demo/lib.Double resolves",
   "call": {"tool": "view", "args": {"pkg": "demo/lib", "sym": "Double"}}}]}
```

A rejection is the agent's error channel and must always answer "what is
the correct next call": diagnostics say what broke, `did_you_mean` lists
bare candidates, `possible_repairs` carries the corrected call whole,
complete and paste-ready. Addressing misses resend the corrected call
(view, query, patch ops, and the sugar mutations), filtered so a repair
never repeats the rejection that produced it; stale generations and
unknown handles repair with the re-view call; a missing required op
argument falls back to the `help` call; an undefined identifier in a
typecheck reject gets the `search` call that locates it. Nothing is
guessed: where no mechanical repair exists, diagnostics stand alone.
Patch rejections say which op index failed; earlier ops in the patch
have no effect. Op arguments decode strictly: a field from another op's
vocabulary rejects at the shape layer naming the field, with the help
call as its repair. An exact resend of a just-rejected call gets an
escalated rejection (`resent`, `escalation`) instead of the same answer
forever. List-returning queries page at 50 entries: `count` is always
the total, truncated responses carry `truncated` and `next_offset`, and
`offset` requests the next page.

## Guarantees

1. An accepted edit introduces no new compiler diagnostic. Problems the
   code already had never block it; they are measured up front and
   reported separately as `pre_existing`.
2. A rejected patch changes nothing: disk, snapshot, or generation.
3. Rename and move prove every rewritten reference still points at the
   intended object. A reference captured by shadowing is rejected even
   when the compiler would accept it.
4. Patches apply whole or not at all; a patch built on a stale
   generation is rejected.
5. Queries see accepted mutations immediately. A patch that reshapes one
   declaration gets that declaration's fresh view back in the response,
   so the next edit needs no extra call; multi-declaration patches say
   why the view was omitted (`views_omitted`).
6. Edits made outside the protocol are detected on the next request and
   trigger a full reload.

## Future, named

- SSA query tier: `writes_to`, `paths_to_effect`, purity/escape facts.
- Multi-module / go.work workspaces (vault sdk/ class of repo).
- Structured-expressions-only mode + constrained decoding integration.
- Batch cross-repo transactions.
- Statement-op coverage growth driven by bench evidence, via the
  versioned catalog.

## Testing

Per-op unit tests against the fixture module; the oracle harness executes
ground-truth-derived patches for every bench task (a task enters the bench
only if the protocol can express it); the raw-vs-semantic bench is the
integration gate and the measure of whether added structure pays.
