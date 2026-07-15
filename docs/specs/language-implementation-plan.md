# ago Language Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the language core from `docs/specs/language.md`: generations, views with node handles, atomic patch transactions, the statement-op vocabulary, test ops, the type-graph query tier, and the consolidated tool surface.

**Architecture:** Everything builds on the existing daemon/snapshot engine (`internal/snapshot`): mutations stay byte-splice + package-level retypecheck + atomic rollback. New layers: a per-declaration generation registry; a view renderer that walks a declaration's AST assigning preorder handles; a patch executor that applies an ordered op list to in-memory file bytes (re-parsing the target declaration after each op so later ops can address `$N` handles), then validates once through the existing retypecheck path and writes atomically. Statement ops are text constructors: they format Go statement text from expression atoms and insert at handle-derived byte offsets.

**Tech Stack:** Go 1.26, go/{ast,parser,token,types,format}, golang.org/x/tools (packages, objectpath, imports). No new dependencies.

## Global Constraints

- Module `github.com/guygrigsby/agent-go`; existing packages `internal/snapshot`, `internal/daemon`, `internal/protocol`, `cmd/ago`.
- Every mutation preserves the spec guarantees: no accepted op leaves a non-compiling tree; rejection changes nothing (disk, snapshot, generation); patches atomic; generations monotonic.
- Rejections always populate `Reason` and, where applicable, `Diagnostics`, `DidYouMean`, `PossibleRepairs`.
- All new ops route through `internal/daemon/daemon.go`'s switch and are exposed via CLI and MCP.
- Test fixture is `internal/snapshot/testdata/demo` copied per-test by the existing `demo(t)` helper; tests never mutate testdata in place.
- gofmt clean; `go test ./...` green after every task; commit after every task (repo commits to main).
- No Anthropic attribution in commits.

## Deferred (follow-up plan, not this one)

`move_decl`, `set_signature`, `remove_param`, `implement_interface`, `add_bench`, project ops (`add_dependency`, `move_file`, `delete_file`, `mod_tidy`, `remove_dependency`), structured expression nodes, SSA query tier, handle migration on stale generations.

---

### Task 1: Generation registry

**Files:**
- Create: `internal/snapshot/generation.go`
- Modify: `internal/snapshot/snapshot.go` (bump on splice), `internal/snapshot/rename.go`, `internal/snapshot/addparam.go`, `internal/snapshot/upsert.go` (include generation in accept responses)
- Test: `internal/snapshot/generation_test.go`

**Interfaces:**
- Produces: `func (s *Snapshot) generation(pkgPath, sym string) int64` (read), `func (s *Snapshot) bumpGenerations(dirty []*packages.Package)` (called with every spliced dirty set and on full load), `func (s *Snapshot) checkGeneration(pkgPath, sym string, want int64) *Reject` — nil when `want` matches or `want == 0` (unspecified).
- Generations are coarse per the spec discussion: any splice or reload bumps every declaration in the affected packages. Key is `pkgPath` (package-level, coarser than per-decl, simplest correct start; the field name in responses is still `generation`).

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/generation_test.go
package snapshot

import "testing"

func TestGenerationBumpsOnMutation(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	g1 := s.generation("demo/lib", "Double")
	if g1 == 0 {
		t.Fatal("generation must be nonzero after load")
	}
	if _, err := s.SetBody("demo/lib", "Double", "return v + v"); err != nil {
		t.Fatal(err)
	}
	g2 := s.generation("demo/lib", "Double")
	if g2 <= g1 {
		t.Fatalf("generation did not advance: %d -> %d", g1, g2)
	}
	// Untouched package unaffected.
	if got := s.generation("demo", "main"); got != 1 {
		t.Fatalf("main pkg generation moved without mutation: %d", got)
	}
}

func TestGenerationCheck(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	g := s.generation("demo/lib", "Double")
	if rej := s.checkGeneration("demo/lib", "Double", g); rej != nil {
		t.Fatalf("current generation rejected: %v", rej)
	}
	if rej := s.checkGeneration("demo/lib", "Double", g+1); rej == nil ||
		rej.Reason != "stale generation: re-view" {
		t.Fatalf("want stale reject, got %v", rej)
	}
	if rej := s.checkGeneration("demo/lib", "Double", 0); rej != nil {
		t.Fatalf("unspecified generation must pass: %v", rej)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/snapshot/ -run Generation -v`
Expected: FAIL, `s.generation undefined`.

- [ ] **Step 3: Implement**

```go
// internal/snapshot/generation.go
package snapshot

import (
	"fmt"

	"golang.org/x/tools/go/packages"
)

// Generations are per-package counters, monotonic for the daemon's life.
// Any splice or reload bumps every package it touched; a view or patch
// built against an older number is stale. Package granularity is coarser
// than the spec's per-declaration ideal but strictly safe; refine only if
// re-view round-trips show up in bench time.
func (s *Snapshot) generation(pkgPath, sym string) int64 {
	return s.gens[pkgPath]
}

func (s *Snapshot) bumpGenerations(dirty []*packages.Package) {
	if s.gens == nil {
		s.gens = map[string]int64{}
	}
	for _, p := range dirty {
		s.gens[p.PkgPath]++
	}
}

func (s *Snapshot) checkGeneration(pkgPath, sym string, want int64) *Reject {
	if want == 0 {
		return nil
	}
	cur := s.generation(pkgPath, sym)
	if cur == want {
		return nil
	}
	return &Reject{Reason: "stale generation: re-view",
		Detail: fmt.Sprintf("%s is at generation %d, patch was built against %d", pkgPath, cur, want)}
}
```

In `snapshot.go`: add `gens map[string]int64` to `Snapshot`; in `load()` after success, bump for every workspace package (so first load yields 1); in `retypecheck` after a successful (non-rolled-back) splice, call `s.bumpGenerations(order)`. In each mutation's accept response map, add `"generation": s.generation(pkgPath, sym)`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/snapshot/ -v`
Expected: all PASS (existing tests unchanged; they ignore the new response key).

- [ ] **Step 5: Commit** — `git commit -m "Add per-package generation registry"`

---

### Task 2: View with node handles

**Files:**
- Create: `internal/snapshot/view.go`
- Test: `internal/snapshot/view_test.go`
- Modify: `internal/daemon/daemon.go` (route `view`), `cmd/ago/main.go` (subcommand), `cmd/ago/mcp.go` (tool)

**Interfaces:**
- Produces: `func (s *Snapshot) View(pkgPath, sym string) (map[string]any, error)` returning `{"status":"ok","generation":N,"text":"...","nodes":M}` where text is the declaration rendered with `nK:` prefixes.
- Produces (internal, consumed by Tasks 3-7): `type nodeTable struct { decl *ast.FuncDecl; file string; nodes map[string]ast.Node; order []string }` and `func (s *Snapshot) nodeTableFor(pkgPath, sym string) (*nodeTable, *Reject)` — handles assigned by preorder walk of the body: every `ast.Stmt` gets a handle; `n1` is the first statement. The walk is deterministic: `handleWalk(body *ast.BlockStmt, visit func(ast.Stmt))` in this file is the single source of handle order for view and patch alike.

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/view_test.go
package snapshot

import (
	"strings"
	"testing"
)

func TestViewHandles(t *testing.T) {
	s := demo(t)
	res, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	text := res["text"].(string)
	if !strings.Contains(text, "n1:") || !strings.Contains(text, "n3:") {
		t.Fatalf("missing handles:\n%s", text)
	}
	if res["generation"].(int64) == 0 {
		t.Fatal("view must carry generation")
	}
	// Handles are stable across identical views.
	res2, _ := s.View("demo/lib", "UseHelper")
	if res2["text"] != text {
		t.Fatal("view not deterministic")
	}
}

func TestViewNonFunction(t *testing.T) {
	s := demo(t)
	res, err := s.View("demo/lib", "Limit")
	if err != nil {
		t.Fatal(err)
	}
	// Non-function decls render without handles.
	if !strings.Contains(res["text"].(string), "Limit") {
		t.Fatalf("got %v", res)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/snapshot/ -run View -v` → FAIL `s.View undefined`.

- [ ] **Step 3: Implement**

```go
// internal/snapshot/view.go
package snapshot

import (
	"fmt"
	"go/ast"
	"os"
	"strings"
)

type nodeTable struct {
	decl  *ast.FuncDecl
	file  string
	nodes map[string]ast.Node
	order []string
}

// handleWalk visits every statement in the body in preorder. It is the
// single definition of handle order; view rendering and patch addressing
// both derive from it.
func handleWalk(body *ast.BlockStmt, visit func(ast.Stmt)) {
	var walk func(list []ast.Stmt)
	walk = func(list []ast.Stmt) {
		for _, st := range list {
			visit(st)
			switch st := st.(type) {
			case *ast.IfStmt:
				walk(st.Body.List)
				if els, ok := st.Else.(*ast.BlockStmt); ok {
					walk(els.List)
				} else if els, ok := st.Else.(*ast.IfStmt); ok {
					walk([]ast.Stmt{els})
				}
			case *ast.ForStmt:
				walk(st.Body.List)
			case *ast.RangeStmt:
				walk(st.Body.List)
			case *ast.SwitchStmt:
				for _, c := range st.Body.List {
					if cc, ok := c.(*ast.CaseClause); ok {
						visit(cc)
						walk(cc.Body)
					}
				}
			case *ast.BlockStmt:
				walk(st.List)
			}
		}
	}
	walk(body.List)
}

func (s *Snapshot) nodeTableFor(pkgPath, sym string) (*nodeTable, *Reject) {
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, &Reject{Reason: "handles exist only inside functions", Detail: objKind(obj)}
	}
	decl, file := findFuncDecl(p, fn)
	if decl == nil || decl.Body == nil {
		return nil, &Reject{Reason: "function declaration not found", Detail: sym}
	}
	nt := &nodeTable{decl: decl, file: file, nodes: map[string]ast.Node{}}
	i := 0
	handleWalk(decl.Body, func(st ast.Stmt) {
		i++
		h := fmt.Sprintf("n%d", i)
		nt.nodes[h] = st
		nt.order = append(nt.order, h)
	})
	return nt, nil
}

// View renders the declaration with a handle prefix on each statement line.
func (s *Snapshot) View(pkgPath, sym string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.ensureFresh(); err != nil {
		return nil, err
	}
	p, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	gen := s.generation(pkgPath, sym)
	// Non-functions: plain source slice, no handles.
	if _, isFn := obj.(*types.Func); !isFn {
		text, err := s.declText(p, obj)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "generation": gen, "text": text, "nodes": 0}, nil
	}
	nt, rej := s.nodeTableFor(pkgPath, sym)
	if rej != nil {
		return nil, rej
	}
	src, err := os.ReadFile(nt.file)
	if err != nil {
		return nil, err
	}
	start := s.fset.Position(nt.decl.Pos()).Offset
	end := s.fset.Position(nt.decl.End()).Offset
	// Annotate: map each handle's statement start line to a prefix.
	prefix := map[int]string{}
	for _, h := range nt.order {
		line := s.fset.Position(nt.nodes[h].Pos()).Line
		if _, taken := prefix[line]; !taken {
			prefix[line] = h + ": "
		}
	}
	firstLine := s.fset.Position(nt.decl.Pos()).Line
	var b strings.Builder
	for i, line := range strings.Split(string(src[start:end]), "\n") {
		if p, ok := prefix[firstLine+i]; ok {
			b.WriteString(p)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return map[string]any{"status": "ok", "generation": gen,
		"text": b.String(), "nodes": len(nt.order)}, nil
}
```

Add `declText` helper (source slice by decl range, reusing `findDeclRange` from upsert.go). Route in daemon (`case "view": res, err = snap.View(req.Pkg, req.Sym)`), CLI subcommand `view`, MCP tool `view {pkg, sym}`. Needed imports: `go/types`.

- [ ] **Step 4: Run tests** — `go test ./internal/snapshot/ -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "Add view: declarations rendered with node handles"`

---

### Task 3: Patch skeleton — transaction over existing ops

**Files:**
- Create: `internal/snapshot/patch.go`
- Test: `internal/snapshot/patch_test.go`
- Modify: `internal/protocol/protocol.go` (add `Ops json.RawMessage`, `Generation int64`, `DryRun bool`), `internal/daemon/daemon.go` (route `patch`), `cmd/ago/main.go`, `cmd/ago/mcp.go`

**Interfaces:**
- Produces: `func (s *Snapshot) Patch(raw []byte) (map[string]any, error)` where raw is the full patch JSON per spec: `{"pkg","sym","generation","dry_run","ops":[{"op":...}]}`.
- Produces (consumed by Tasks 4-8): the op registry —

```go
type patchCtx struct {
	s        *Snapshot
	pkg, sym string          // defaults for ops that omit them
	src      map[string][]byte // working copy of file contents
	handles  map[string]string // "$1" -> handle assigned by op 1
}
type patchOp interface {
	name() string
	apply(ctx *patchCtx, args json.RawMessage) *Reject
}
var opRegistry = map[string]func() patchOp{}
```

- Ops apply to `ctx.src` (in-memory bytes) only. After the op list, Patch writes all touched files, runs the existing dirty-set validation (`dirtyByFiles` + `affected` + `retypecheck`), and on any failure restores every file and re-splices — the same rollback discipline as rename. `dry_run` runs everything then always restores, reporting what would have happened.
- v1 ops registered in this task: `rename`, `set_body`, `add_param`, `upsert_decl` (each delegates to the existing implementation for single-op patches; multi-op patches with these delegate sequentially with validation deferred to the end — implemented by giving the existing four a `validate bool` internal variant is NOT done; instead these four run as single-op fast paths and multi-op support arrives with the handle ops in Task 4, which are natively `ctx.src`-based. A patch mixing legacy ops with more than one op total is rejected `"not yet composable"` — removed in Task 8).

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/patch_test.go
package snapshot

import (
	"strings"
	"testing"
)

func TestPatchSingleRename(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"rename","to":"Twice"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.Inspect("demo/lib", "Twice"); err != nil {
		t.Fatal(err)
	}
}

func TestPatchStaleGeneration(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double","generation":999,
		"ops":[{"op":"rename","to":"Twice"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.HasPrefix(rej.Reason, "stale generation") {
		t.Fatalf("want stale reject, got %v", err)
	}
}

func TestPatchUnknownOp(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"frobnicate"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown op" || len(rej.DidYouMean) == 0 {
		t.Fatalf("want unknown-op reject with catalog suggestions, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/snapshot/ -run Patch -v` → FAIL.

- [ ] **Step 3: Implement**

`Patch` parses the envelope, runs `checkGeneration(pkg, sym, generation)`, then:
single legacy op → dispatch to the existing method (map `"rename"` → `s.Rename` etc., translating args via a small struct per op); response gains `"ops_applied": 1`. Unknown op name → `&Reject{Reason: "unknown op", Detail: name, DidYouMean: nearestOps(name)}` where `nearestOps` does the same substring match as `suggestSymbols` over registry keys plus legacy names. Multi-op → `"not yet composable"` reject (temporary, Task 8 removes). `dry_run` on the legacy path: run against a copy? No — legacy ops write; for v1 dry_run with legacy ops is rejected `"dry_run requires composable ops"` (Task 8 removes). Keep the envelope struct and registry exactly as the Interfaces block defines so later tasks slot in.

- [ ] **Step 4: Run tests** — expect PASS, all suites.
- [ ] **Step 5: Commit** — `git commit -m "Add patch envelope: generation check, op dispatch, catalog suggestions"`

---

### Task 4: Handle-addressed insertion engine

**Files:**
- Create: `internal/snapshot/ops_stmt.go`
- Test: `internal/snapshot/ops_stmt_test.go`
- Modify: `internal/snapshot/patch.go` (composable path: ctx.src pipeline, end-of-list validation, atomic write, dry_run)

**Interfaces:**
- Produces (consumed by Tasks 5-7): 

```go
// insertStmt places rendered statement text relative to a handle and
// re-parses the declaration so subsequent ops see fresh handles.
// where: "before"|"after"|"first"|"last" ("first"/"last" need a block-owning handle).
func (ctx *patchCtx) insertStmt(at, where, stmtText string) (newHandle string, rej *Reject)
// exprAtom parses and returns gofmt'd expression text or a Reject with
// in-scope did_you_mean candidates.
func (ctx *patchCtx) exprAtom(src string) (string, *Reject)
```

- The composable pipeline in patch.go: load target file bytes into ctx.src once; each op edits bytes and re-parses the single target decl (parser on the file bytes, then `nodeTableFor`-equivalent against the parsed copy — factor `buildNodeTable(decl)` out of Task 2's code for reuse against non-snapshot ASTs); after the last op, `format.Source` each touched file, write all, validate via `dirtyByFiles`+`affected`+`retypecheck`, rollback-on-reject identical to rename; on accept `noteWrite` + `bumpGenerations`. dry_run: identical then unconditional restore, response `{"status":"ok","dry_run":true,"would":"accepted"}` or the reject.

- [ ] **Step 1: Write the failing test**

```go
// internal/snapshot/ops_stmt_test.go
package snapshot

import (
	"os"
	"strings"
	"testing"
)

func TestPatchInsertAfterHandle(t *testing.T) {
	s := demo(t)
	// UseHelper body: n1 assign, n2 blank-assign, n3 return.
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"extra","rhs":"h(1)","define":true},
		       {"op":"add_call","at":"n3","where":"before","expr":"_ = extra"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" || res["ops_applied"].(int) != 2 {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(res["files"].([]string)[0])
	if !strings.Contains(string(b), "extra := h(1)") {
		t.Fatalf("insert missing:\n%s", b)
	}
}

func TestPatchAtomicOnLaterFailure(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(s.dir + "/lib/use.go")
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"x","rhs":"1","define":true},
		       {"op":"add_call","at":"n99","where":"before","expr":"println(x)"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown handle" || !strings.Contains(rej.Detail, "op 2") {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("failed patch left partial edits")
	}
}

func TestPatchDryRun(t *testing.T) {
	s := demo(t)
	orig, _ := os.ReadFile(s.dir + "/lib/use.go")
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper","dry_run":true,
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"x","rhs":"1","define":true}]}`))
	if err != nil || res["dry_run"] != true || res["would"] != "accepted" {
		t.Fatalf("got %v %v", res, err)
	}
	after, _ := os.ReadFile(s.dir + "/lib/use.go")
	if string(orig) != string(after) {
		t.Fatal("dry_run wrote")
	}
}
```

- [ ] **Step 2: Verify failure** — FAIL (`add_assign` unknown / "not yet composable").

- [ ] **Step 3: Implement**

`insertStmt`: resolve handle in the current parse of ctx.src (unknown → `&Reject{Reason: "unknown handle", Detail: fmt.Sprintf("op %d: %s", ctx.opIndex, at), DidYouMean: <nearest existing handles>}`); compute insertion offset — `before`: line start of node.Pos; `after`: after node.End (plus newline); `first`/`last`: just inside the block braces of an owning node (reject non-block handles); splice `stmtText+"\n"`; re-parse the file copy; rebuild the table; the newly inserted statement is identified by position range and returned as its fresh handle. `exprAtom`: `parser.ParseExpr`; on parse error reject `"expression does not parse"`; on success return `format`d text. Type errors surface at end-of-list validation with the op index attributed by tracking which op touched which line range (nearest-op attribution is acceptable: record per-op edited ranges, map diagnostic positions).

Register `add_assign {lhs, rhs, define}` (renders `lhs = rhs` / `lhs := rhs`) and `add_call {expr}` (renders the expression statement) — the two ops the tests need; both two-liners over `insertStmt`+`exprAtom`.

- [ ] **Step 4: Run tests** — PASS.
- [ ] **Step 5: Commit** — `git commit -m "Add composable patch pipeline and handle-addressed insertion"`

---

### Task 5: Statement constructors, linear set

**Files:**
- Modify: `internal/snapshot/ops_stmt.go`
- Test: `internal/snapshot/ops_stmt_test.go` (extend)

**Interfaces:**
- Consumes: `insertStmt`, `exprAtom` from Task 4.
- Produces ops: `add_return {exprs []string}`, `add_defer {expr}`, `add_go {expr}`, `delete_node {at}` (statement or empty block/case only; reject non-empty block deletion with the child handles listed).

- [ ] **Step 1: Failing tests** — one per op in the established pattern; complete examples:

```go
func TestAddReturn(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"delete_node","at":"n3"},
		       {"op":"add_return","at":"n2","where":"after","exprs":["helper(3)"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddReturnArityChecked(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_return","at":"n3","where":"before","exprs":["1","2"]}]}`))
	rej, ok := err.(*Reject)
	if !ok || len(rej.Diagnostics) == 0 {
		t.Fatalf("want typecheck reject on arity, got %v", err)
	}
}

func TestDeleteNodeRejectsNonEmptyBlock(t *testing.T) {
	s := demo(t)
	// n1 in Double after Task 4 fixture state is a plain statement; craft an
	// if via patch, then try deleting its handle while populated.
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"true"},
		       {"op":"add_call","at":"$1","where":"first","expr":"println(1)"},
		       {"op":"delete_node","at":"$1"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "block is not empty" {
		t.Fatalf("got %v", err)
	}
}
```

(The third test also exercises Task 6's `add_if` and `$N`; place it in the file now guarded until Task 6 lands, or land Tasks 5 and 6 in one commit series — executor's choice, both tasks are in this plan.)

- [ ] **Step 2: Verify failure.**
- [ ] **Step 3: Implement** — each op renders text and calls `insertStmt`; `add_return` renders `return e1, e2`; arity/type errors come from end-of-list validation (no special casing). `delete_node` splices out the node's byte range; for block-owning statements first checks child count via the node table.
- [ ] **Step 4: Run tests** — PASS.
- [ ] **Step 5: Commit** — `git commit -m "Add linear statement ops: return, defer, go, delete_node"`

---

### Task 6: Block constructors and $N handles

**Files:**
- Modify: `internal/snapshot/ops_stmt.go`, `internal/snapshot/patch.go` ($N resolution)
- Test: `internal/snapshot/ops_stmt_test.go` (extend)

**Interfaces:**
- Produces ops: `add_if {cond, else?}`, `add_for {cond?}` (v1: condition-only and infinite forms; range form via `range` arg string), `add_switch {tag?}`, `add_case {at, exprs? | default}`.
- Produces: `$N` in any `at` resolves to the handle returned by op N (1-based). Constructors record their new handle into `ctx.handles["$«index»"]`.

- [ ] **Step 1: Failing test**

```go
func TestBlockConstructorsAndDollarRefs(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_if","at":"n1","where":"after","cond":"helper(1) > 0"},
		       {"op":"add_call","at":"$1","where":"first","expr":"println(\"pos\")"},
		       {"op":"add_switch","at":"$1","where":"after","tag":"helper(2)"},
		       {"op":"add_case","at":"$3","exprs":["1","2"]},
		       {"op":"add_call","at":"$4","where":"first","expr":"println(\"small\")"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["ops_applied"].(int) != 5 {
		t.Fatalf("got %v", res)
	}
}
```

- [ ] **Step 2: Verify failure.**
- [ ] **Step 3: Implement** — constructors render `if cond {\n}\n` (plus `else {}` when requested), `for cond {}`, `switch tag {}`, `case e1, e2:`; after re-parse the new node is located by inserted range and its handle recorded. `$N` resolution in patch.go before each apply: reject `"unknown $ref"` if N ≥ current index or the op produced no handle.
- [ ] **Step 4: Run tests.**
- [ ] **Step 5: Commit** — `git commit -m "Add block constructors and intra-patch $N handles"`

---

### Task 7: Mutating statement ops

**Files:**
- Modify: `internal/snapshot/ops_stmt.go`
- Test: `internal/snapshot/ops_stmt_test.go` (extend)

**Interfaces:**
- Produces ops: `set_cond {at, expr}` (if/for/case), `replace_expr {at, expr}` (v1 scope: the node's condition or a whole expression statement — per-slot sub-expression handles are future), `wrap_stmts {from, to, with, cond?}` (`with: "if"|"for"|"block"`), `wrap_error {at, message}`.

- [ ] **Step 1: Failing tests** — complete versions of these two plus one per remaining op:

```go
func TestWrapError(t *testing.T) {
	s := demo(t)
	// Give the fixture an error-returning call to wrap.
	if _, err := s.UpsertDecl("demo/lib", "func fallible() (int, error) {\n\treturn 1, nil\n}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDecl("demo/lib", "func Caller() (int, error) {\n\tn, _ := fallible()\n\treturn n, nil\n}"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Caller",
		"ops":[{"op":"wrap_error","at":"n1","message":"calling fallible"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.View("demo/lib", "Caller")
	text := res["text"].(string)
	if !strings.Contains(text, "if err != nil") || !strings.Contains(text, `calling fallible: %w`) {
		t.Fatalf("wrap_error shape wrong:\n%s", text)
	}
}

func TestWrapStmts(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"wrap_stmts","from":"n1","to":"n2","with":"if","cond":"helper(9) > 0"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.View("demo/lib", "UseHelper")
	if !strings.Contains(res["text"].(string), "if helper(9) > 0 {") {
		t.Fatalf("got:\n%s", res["text"])
	}
}
```

- [ ] **Step 2: Verify failure.**
- [ ] **Step 3: Implement** — `set_cond`/`replace_expr` splice the expression's byte range (condition range from the node's AST fields). `wrap_stmts` validates from/to are siblings in order (reject otherwise, listing the enclosing block's handles), splices `with`-header + `{` before and `}` after. `wrap_error` requires `at` be an assignment whose last LHS is `_` or `err` with a call RHS, or an expression-statement call returning error; rewrites to `..., err := call(...)` / reuses `err`, inserts `if err != nil { return <zero values>, fmt.Errorf("message: %w", err) }` with zero values derived from the enclosing signature; rejects `"enclosing function does not return error"` otherwise.
- [ ] **Step 4: Run tests.**
- [ ] **Step 5: Commit** — `git commit -m "Add mutating statement ops: set_cond, replace_expr, wrap_stmts, wrap_error"`

---

### Task 8: Remaining composable decl ops + legacy composition

**Files:**
- Create: `internal/snapshot/ops_decl.go`
- Modify: `internal/snapshot/patch.go` (drop "not yet composable"), `internal/snapshot/rename.go`/`addparam.go`/`upsert.go` (extract text-edit cores that operate on ctx.src)
- Test: `internal/snapshot/ops_decl_test.go`

**Interfaces:**
- Produces ops: `delete_decl {pkg, sym}` (reject with reference list while refs remain), `set_doc {pkg, sym, text}`, `add_field {pkg, sym, name, type, tag?}`, `remove_field {pkg, sym}` (reject while referenced).
- Produces: rename/add_param/upsert_decl/set_body usable inside multi-op patches: their edit computation is factored to produce `[]edit` against ctx.src instead of writing directly; single-op sugar paths now call through the same core. The interface+impls atomic rename becomes expressible: `[{"op":"rename","pkg":P,"sym":"Iface.M","to":"N"},{"op":"rename","pkg":P,"sym":"Impl.M","to":"N"}]` validated as one unit.

- [ ] **Step 1: Failing tests** — `TestPatchInterfaceImplRenameAtomic` (fixture gains a small interface + impl via upsert in the test; single renames rejected, paired patch accepted), `TestDeleteDeclRejectsWhileReferenced`, `TestSetDoc`, `TestAddRemoveField` — written out in full in the established style.
- [ ] **Step 2: Verify failure.**
- [ ] **Step 3: Implement.** The extraction is the meat: `renameEdits(...) ([]edit, *Reject)` etc.; end-of-list resolution proof runs once for all renames in the patch.
- [ ] **Step 4: Run tests, including the whole existing suite.**
- [ ] **Step 5: Commit** — `git commit -m "Composable decl ops; atomic multi-rename patches"`

---

### Task 9: Query tier — callers, callees, implementations, doc

**Files:**
- Create: `internal/snapshot/queries.go`
- Test: `internal/snapshot/queries_test.go`
- Modify: `internal/daemon/daemon.go`, `cmd/ago/main.go`, `cmd/ago/mcp.go` (single `query {kind,...}` routing; keep search/inspect/refs working under it and as-is)

**Interfaces:**
- Produces: `func (s *Snapshot) Callers(pkg, sym string) (map[string]any, error)` — enclosing function for every call-shaped reference (reuses `references` + `enclosingCall` from addparam.go; each hit reports the calling function's pkg.sym and position). `Callees(pkg, sym)` — walk the decl body for call expressions, resolve via TypesInfo.Uses. `Implementations(pkg, sym)` — interface: scan workspace named types for `types.Implements` (value and pointer receiver); concrete type: scan workspace interfaces it satisfies. `Doc(pkg, sym)` — decl doc comment via syntax.
- MCP: one `query` tool `{kind, pkg?, sym?, q?}`.

- [ ] **Step 1: Failing tests** (fixture gains `type Putter interface { Put(int) }` via testdata edit — update `TestRefs` expectations if counts shift; complete test code for all four kinds in the established pattern, e.g.):

```go
func TestImplementations(t *testing.T) {
	s := demo(t)
	res, err := s.Query("implementations", "demo/lib", "Putter", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(res["types"]), "Store") {
		t.Fatalf("got %v", res)
	}
}
```

- [ ] **Steps 2-4: fail, implement, pass** — `Query(kind, pkg, sym, q)` dispatching to the four new plus existing three.
- [ ] **Step 5: Commit** — `git commit -m "Add query tier: callers, callees, implementations, doc"`

---

### Task 10: Test ops

**Files:**
- Create: `internal/snapshot/ops_testgen.go`
- Test: `internal/snapshot/ops_testgen_test.go`

**Interfaces:**
- Produces ops: `add_test {pkg, target, name?}` → scaffolds into `<declfile>_test.go` (created on demand, internal test package when no precedent) the canonical skeleton; returns the table handle. `add_test_case {pkg, test, name, args []string, want []string}` → appends a typed row (addresses the table by test name — handles work too but name is the common path). `set_test_case`/`remove_test_case {pkg, test, case}`.
- Canonical skeleton, fixed (target `func Double(v int) int` shown):

```go
func TestDouble(t *testing.T) {
	tests := []struct {
		name string
		v    int
		want int
	}{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Double(tt.v); got != tt.want {
				t.Errorf("Double(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
```

Multi-result targets compare each; error results get `wantErr bool` + `(err != nil) != tt.wantErr`. Assertion-library detection (testify) is implemented by checking existing `_test.go` imports in the package; testify variant uses `require.Equal`. Non-comparable result types reject with `"result type not comparable; write the test with upsert_decl"`.

- [ ] **Step 1: Failing tests** — `TestAddTestScaffold` (creates lib_test.go, compiles, skeleton contains the case struct), `TestAddTestCase` (row appears, values typechecked: a `want []string` mismatch rejects), `TestAddTestCaseWrongType` (reject with diagnostics), `TestRemoveTestCase`. Full code in the established pattern.
- [ ] **Steps 2-4: fail, implement, pass.** Scaffold generation goes through the upsert pipeline (it is an upsert into a `_test.go` path — reuse `UpsertDecl` internals with an explicit file target). Case rows are `insertStmt`-class byte edits inside the composite literal, validated by the normal pipeline.
- [ ] **Step 5: Commit** — `git commit -m "Add test ops: table-driven scaffold and typed case rows"`

---

### Task 11: The test tool

**Files:**
- Create: `internal/snapshot/testrun.go`
- Test: `internal/snapshot/testrun_test.go`
- Modify: `internal/daemon/daemon.go`, `cmd/ago/main.go`, `cmd/ago/mcp.go`

**Interfaces:**
- Produces: `func (s *Snapshot) TestRun(pkg, run string) (map[string]any, error)` — executes `go test -count=1 -timeout 10m -json [-run run] <pkg|./...>` in the workspace, parses the `-json` event stream, returns `{"status":"ok","pass":bool,"tests":[{"name","package","pass","elapsed_s","output"?}],"failed":N}` with output only for failures, truncated to 2000 bytes each. Read-only: takes no snapshot lock beyond resolving the dir; runs while the daemon serves queries.
- MCP tool `test {pkg?, run?}`.

- [ ] **Step 1: Failing test** — `TestTestRun`: scaffold a passing test via Task 10's op, run `TestRun("demo/lib","")`, expect pass=true and the test listed; then upsert a failing test and expect pass=false with its output captured.
- [ ] **Steps 2-4: fail, implement, pass.**
- [ ] **Step 5: Commit** — `git commit -m "Add test tool: scoped go test with structured results"`

---

### Task 12: help catalog and surface consolidation

**Files:**
- Create: `internal/snapshot/help.go` (catalog data derived from the op registry: name, args with types, one example each, version string)
- Modify: `cmd/ago/mcp.go` — final tool surface exactly per spec: `status, help, query, view, patch, test` + sugar `rename, set_body, add_param, upsert_decl`; remove `search/inspect/refs` as separate tools (they live under `query`). `cmd/ago/main.go` keeps all CLI subcommands (human surface is allowed to be wider). `cmd/ago/init.go` — AGENTS.md rewritten to teach the final surface with a worked patch example.
- Test: `internal/snapshot/help_test.go` (catalog lists every registered op; every op's example parses as valid patch JSON), plus a scripted MCP session test extension in `cmd/ago` if one exists (it does not — add `cmd/ago/mcp_test.go` driving `runMCP` over an in-memory pipe: initialize, tools/list must return exactly the 10 tools, one `patch` tools/call round-trip against a temp `ago init` project).

- [ ] Steps: failing test → implement → pass → commit `git commit -m "Add help catalog; consolidate MCP surface to the spec's ten tools"`.

---

### Task 13: Oracle and docs closure

**Files:**
- Modify: `docs/specs/plan.md` (status), `docs/specs/protocol.md` (point op catalog at language.md, list implemented surface), `README.md` (surface table)
- Test: full suite + real-path verification on boundary (manual commands recorded in the commit message): `view` a real function, a 3-op statement patch accepted, stale-generation patch rejected, `query implementations` on a boundary interface, `test` on a small package.

- [ ] Steps: run `go test ./...`; run the boundary real-path list above via CLI; fix anything found; update docs; commit `git commit -m "Language core complete: docs and real-path verification"`.

---

## Self-review notes

- Spec coverage: view/handles/generations (T1-2), patch atomicity+dry_run+$N (T3,4,6), statement ops (T4-7 cover the spec table minus `add_for` range form — included in T6 args), composable decl ops incl. atomic multi-rename (T8), delete/doc/fields (T8), query tier (T9), test ops + canonical skeleton + convention detection (T10), test tool (T11), help + ten-tool surface + init teaching (T12). Deferred list matches the spec's Future section plus the explicitly named decl/project ops.
- Known simplifications, stated where they bind: package-granular generations (T1), replace_expr limited to condition/whole-expression slots (T7), non-comparable test results rejected (T10). Each is a documented ceiling, not a silent gap.
