# Authoring Bench Tier 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the `author` bench kind: greenfield tasks mined from real commits with a coverage gate, both arms on the first-party driver, oracle replay through the op surface, guard lattice rows. Spec: `docs/specs/authoring-bench.md`. ADR: `docs/adr/0006-authoring-bench-arm.md`. Bead: `agent-go-zc5`.

**Architecture:** Two tracks. Track A gives `ago agent` a bench-only raw surface (ungated `write_file`, `read_file`, `test`, dispatch gated, no ops). Track B teaches the bench the `author` kind: manifest spec carrying frozen test-file bytes, a predicate that checks the freeze plus an authored implementation, episode dispatch through the driver instead of opencode, an oracle case that replays ground-truth decls as one atomic `upsert_decl` patch, and a structural miner with a measured coverage gate. Tracks A and B are independent until Task 6; a subagent dispatcher can run Tasks 1 and 3 concurrently.

**Tech Stack:** Go stdlib only (`go/parser`, `go/ast`, `os/exec` git plumbing). No new dependencies.

## Global Constraints

- Strict TDD: write the failing test, run it, watch it fail, then implement. Every task below is structured that way; do not reorder.
- No em or en dashes anywhere: code, comments, docs, commit messages.
- Commit messages: terse, verb-first, no attribution footers, no Claude credits.
- `gofmt -l .` must be empty before every commit.
- `for range n` / `for i := range n`, never three-clause count loops.
- Bench evidence in `bench/results/` is append-only; never rewrite committed runs.
- No sidecar scripts; anything executable is a Go subcommand.
- Full gate before the final push: `go test ./...` green (snapshot suite takes ~90s).
- The bench package's test-file helpers live in `bench/bench_test.go`; non-test helpers shared with tests live in `bench/*.go` (see `bench/compose.go` for the pattern).

---

### Task 1: Raw file tools (Track A)

**Files:**
- Modify: `internal/agent/files.go`
- Test: `internal/agent/files_test.go`

**Interfaces:**
- Consumes: existing `FileTools`, `NewFileTools(root string) *FileTools`.
- Produces: `NewRawFileTools(root string) *FileTools` whose `Call("write_file", ...)` accepts `.go` paths. `NewFileTools` behavior unchanged. Task 2 consumes both constructors.

- [ ] **Step 1: Write the failing test** (append to `internal/agent/files_test.go`):

```go
func TestRawFileToolsWritesGo(t *testing.T) {
	dir := t.TempDir()
	f := NewRawFileTools(dir)
	out, isErr := f.Call("write_file", map[string]any{"path": "echo.go", "content": "package echo\n"})
	if isErr {
		t.Fatalf("raw write_file rejected a .go path: %s", out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "echo.go"))
	if err != nil || string(b) != "package echo\n" {
		t.Fatalf("raw write did not land: %v %q", err, b)
	}
	// The raw surface still cannot escape the workspace.
	if _, isErr := f.Call("write_file", map[string]any{"path": "../out.go", "content": "x"}); !isErr {
		t.Fatal("raw write_file must still reject workspace escapes")
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./internal/agent -run TestRawFileToolsWritesGo`
Expected: FAIL, `undefined: NewRawFileTools`.

- [ ] **Step 3: Implement** in `internal/agent/files.go`: add an `allowGo` field and constructor, gate the existing rejection on it. The struct doc comment grows one sentence; keep the existing sentences.

```go
type FileTools struct {
	root    string
	allowGo bool
}

func NewFileTools(root string) *FileTools { return &FileTools{root: root} }

// NewRawFileTools drops the .go gate: the bench's raw arm measures what a
// model does WITHOUT the protocol, so it writes Go source directly. Never
// served outside the bench raw surface (ADR 0006).
func NewRawFileTools(root string) *FileTools { return &FileTools{root: root, allowGo: true} }
```

and in `Call`, change the write_file gate to:

```go
	if !f.allowGo && strings.EqualFold(filepath.Ext(rel), ".go") {
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/agent`
Expected: PASS (existing `files_test.go` proves the semantic gate still rejects).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/files.go internal/agent/files_test.go
git commit -m "Raw file tools: bench raw arm writes Go directly, semantic gate and escape checks unchanged"
```

---

### Task 2: Driver raw surface and flags (Track A)

**Files:**
- Modify: `cmd/ago/agent.go`, `cmd/ago/main.go:280-301`
- Test: `cmd/ago/agent_test.go`

**Interfaces:**
- Consumes: `agent.NewFileTools`, `agent.NewRawFileTools` (Task 1), existing `mcpTools()`, `mcpCall(dir, name, args)`.
- Produces: `agentToolDefs(surface string) []agent.ToolDef`, `newAgentTools(dir, surface string) *agentTools`, `runAgent(dir, task, profile, endpoint, model string, maxSteps int, cap time.Duration, surface, transcript string) error`, CLI flags `--surface` (default `semantic`) and `--transcript` (default empty, meaning `.ago/sessions/<ts>.jsonl`). Task 6 execs `ago agent --surface <mode> --transcript <path>`.

- [ ] **Step 1: Write the failing tests** (append to `cmd/ago/agent_test.go`). This is the surface guard ADR 0006 names: which tools each arm serves is asserted by test, not prose. Note: use the exact tool names `mcpTools()` emits; if they carry a prefix, adjust the literals to match (check `cmd/ago/mcp_test.go`).

```go
func TestAgentSurfaces(t *testing.T) {
	names := func(defs []agent.ToolDef) map[string]bool {
		m := map[string]bool{}
		for _, d := range defs {
			m[d.Name] = true
		}
		return m
	}
	raw := names(agentToolDefs("raw"))
	want := map[string]bool{"read_file": true, "write_file": true, "test": true}
	if !maps.Equal(raw, want) {
		t.Fatalf("raw surface drifted from ADR 0006: %v", raw)
	}
	sem := names(agentToolDefs("semantic"))
	if !sem["upsert_decl"] || !sem["read_file"] || !sem["write_file"] {
		t.Fatalf("semantic surface lost a tool: %v", sem)
	}
	if sem["shell"] || sem["bash"] {
		t.Fatalf("no shell on any surface: %v", sem)
	}
}

func TestRawSurfaceGatesDispatch(t *testing.T) {
	tools := newAgentTools(t.TempDir(), "raw")
	out, isErr := tools.Call("rename", map[string]any{"pkg": "x", "sym": "A", "to": "B"})
	if !isErr || !strings.Contains(out, "raw surface") {
		t.Fatalf("raw dispatch must reject op calls by name: %q %v", out, isErr)
	}
	if out, isErr := tools.Call("write_file", map[string]any{"path": "a.go", "content": "package a\n"}); isErr {
		t.Fatalf("raw write_file must land .go: %s", out)
	}
}
```

- [ ] **Step 2: Run them, watch them fail**

Run: `go test ./cmd/ago -run 'TestAgentSurfaces|TestRawSurfaceGatesDispatch'`
Expected: FAIL, `agentToolDefs` takes no argument / `newAgentTools` takes one argument.

- [ ] **Step 3: Implement** in `cmd/ago/agent.go`:

1. `agentToolDefs(surface string)`: for `"raw"`, return only the `test` def found by name in `mcpTools()` plus `read_file` and an ungated `write_file` def (description: "Write any file in the workspace, Go source included."). Otherwise return the current full list.
2. `agentTools` gains `surface string`; `newAgentTools(dir, surface string)` picks `agent.NewRawFileTools` when raw. `Call` gates dispatch:

```go
func (t *agentTools) Call(name string, args map[string]any) (string, bool) {
	if name == "read_file" || name == "write_file" {
		return t.files.Call(name, args)
	}
	if t.surface == "raw" && name != "test" {
		return "tool " + name + " is not on the raw surface; use read_file, write_file, and test", true
	}
	return mcpCall(t.dir, name, args)
}
```

3. Raw system prompt const, mirroring `promptRaw`'s workflow in `bench/prompts.go` but for the file tools (no shell exists):

```go
const agentRawPrompt = `You are a Go authoring agent. Use read_file to inspect the workspace, write_file to create or change any file, Go source included, and test to check your work. A failing test names exactly what to fix; adjust and retry.

When the task is complete, answer with a short summary of what changed and stop calling tools.`
```

4. `runAgent` gains `surface, transcript string` parameters: select prompt and tools by surface; when `transcript` is non-empty create that exact path (MkdirAll its dir) instead of the sessions default.
5. `cmd/ago/main.go`: two new flags on `agentCmd`, threaded through:

```go
agentCmd.Flags().StringVar(&agentSurface, "surface", "semantic", "tool surface: semantic (ops) or raw (bench-only, ungated writes)")
agentCmd.Flags().StringVar(&agentTranscript, "transcript", "", "write the JSONL transcript here instead of .ago/sessions")
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./cmd/ago`
Expected: PASS. If `cmd/ago/guards_test.go` or `readme_test.go` fails, it names the doc file to fix; fix it in this task (no op was added, so no `catalogVersion` bump).

- [ ] **Step 5: Commit**

```bash
git add cmd/ago/agent.go cmd/ago/agent_test.go cmd/ago/main.go
git commit -m "ago agent grows a bench-only raw surface: ungated write_file plus test, dispatch gated by name, transcript path flag"
```

---

### Task 3: Manifest author kind (Track B)

**Files:**
- Modify: `bench/manifest.go`
- Test: `bench/manifest_test.go`

**Interfaces:**
- Produces: `AuthorSpec`, `AuthorFile`, `Manifest.Author *AuthorSpec`, `HasSpecs` arm for `"author"`. Tasks 4 through 8 consume these exact shapes.

- [ ] **Step 1: Write the failing test** (append to `bench/manifest_test.go`):

```go
func TestHasSpecsAuthor(t *testing.T) {
	m := Manifest{Kind: "author"}
	if m.HasSpecs() {
		t.Fatal("author without a spec must not count")
	}
	m.Author = &AuthorSpec{Pkg: "example.com/m/echo", Dir: "echo", Coverage: 95.0,
		TestFiles: []AuthorFile{{Path: "echo/echo_test.go", Content: "package echo\n"}}}
	if !m.HasSpecs() {
		t.Fatal("complete author spec must count")
	}
	m.Author.TestFiles[0].Content = ""
	if m.HasSpecs() {
		t.Fatal("empty spec content scores vacuously and must not count")
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./bench -run TestHasSpecsAuthor`
Expected: FAIL, `undefined: AuthorSpec`.

- [ ] **Step 3: Implement** in `bench/manifest.go`:

```go
// AuthorFile is one frozen spec test file: the bytes the harness lands in
// the worktree and the bytes scoring demands are still there. Byte
// equality IS the freeze; no separate hash to drift.
type AuthorFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// AuthorSpec is a tier 1 authoring task: the ground-truth commit's tests
// are the spec, the model authors the package that passes them
// (docs/specs/authoring-bench.md). Coverage records the ground-truth
// suite's own statement coverage at mining time; the gate lives in the
// extractor, the number rides here as evidence.
type AuthorSpec struct {
	Pkg       string       `json:"pkg"`
	Dir       string       `json:"dir"`
	Coverage  float64      `json:"coverage"`
	TestFiles []AuthorFile `json:"test_files"`
}
```

Add `Author *AuthorSpec \`json:"author,omitempty"\`` to `Manifest` after `Moves`, and the `HasSpecs` arm:

```go
	case "author":
		if m.Author == nil || m.Author.Pkg == "" || m.Author.Dir == "" || len(m.Author.TestFiles) == 0 {
			return false
		}
		for _, f := range m.Author.TestFiles {
			if f.Path == "" || f.Content == "" {
				return false
			}
		}
		return true
```

- [ ] **Step 4: Run the tests**

Run: `go test ./bench -run TestHasSpecs`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add bench/manifest.go bench/manifest_test.go
git commit -m "Manifest author kind: frozen spec test files, coverage evidence, HasSpecs arm"
```

---

### Task 4: Spec placement and freeze check (Track B)

**Files:**
- Create: `bench/author.go`
- Test: `bench/author_test.go` (create)

**Interfaces:**
- Consumes: `AuthorSpec`, `AuthorFile` (Task 3).
- Produces: `writeAuthorSpec(wt string, spec *AuthorSpec) error` and `specFilesIntact(wt string, spec *AuthorSpec) (reason string, ok bool)`; `reason` is empty when ok, otherwise names the first diverging path. Tasks 5 and 6 consume both.

- [ ] **Step 1: Write the failing test** (`bench/author_test.go`):

```go
package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecFreeze(t *testing.T) {
	wt := t.TempDir()
	spec := &AuthorSpec{Pkg: "example.com/m/echo", Dir: "echo",
		TestFiles: []AuthorFile{{Path: "echo/echo_test.go", Content: "package echo\n"}}}
	if err := writeAuthorSpec(wt, spec); err != nil {
		t.Fatal(err)
	}
	if reason, ok := specFilesIntact(wt, spec); !ok {
		t.Fatalf("freshly placed spec must be intact: %s", reason)
	}
	// Mutation is a spec violation and the reason names the file.
	mutated := filepath.Join(wt, "echo", "echo_test.go")
	os.WriteFile(mutated, []byte("package echo // gutted\n"), 0o644)
	if reason, ok := specFilesIntact(wt, spec); ok || !strings.Contains(reason, "echo/echo_test.go") {
		t.Fatalf("mutated spec must fail naming the file, got ok=%v reason=%q", ok, reason)
	}
	// Deletion too.
	os.Remove(mutated)
	if _, ok := specFilesIntact(wt, spec); ok {
		t.Fatal("deleted spec file must fail the freeze")
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./bench -run TestSpecFreeze`
Expected: FAIL, `undefined: writeAuthorSpec`.

- [ ] **Step 3: Implement** (`bench/author.go`):

```go
package bench

// Author kind helpers: the ground-truth commit's tests are the spec
// (docs/specs/authoring-bench.md). The harness lands them pre-episode and
// scoring demands them back byte for byte; deleting or editing the test
// is the shortest path to green, so the freeze fails the episode
// outright.

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeAuthorSpec lands the frozen spec tests in a fresh worktree.
func writeAuthorSpec(wt string, spec *AuthorSpec) error {
	for _, f := range spec.TestFiles {
		abs := filepath.Join(wt, f.Path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// specFilesIntact reports whether every spec test file still carries its
// frozen bytes; reason names the first divergence.
func specFilesIntact(wt string, spec *AuthorSpec) (string, bool) {
	for _, f := range spec.TestFiles {
		b, err := os.ReadFile(filepath.Join(wt, f.Path))
		if err != nil {
			return fmt.Sprintf("spec test %s is gone: %v", f.Path, err), false
		}
		if string(b) != f.Content {
			return fmt.Sprintf("spec test %s was modified", f.Path), false
		}
	}
	return "", true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./bench -run TestSpecFreeze`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add bench/author.go bench/author_test.go
git commit -m "Author spec freeze: harness lands the ground-truth tests, scoring demands them back byte for byte"
```

---

### Task 5: Author predicate and scoring wiring (Track B)

**Files:**
- Modify: `bench/bench_test.go` (predicates map near line 627, `score` near line 713, `episode` near line 279)

**Interfaces:**
- Consumes: `specFilesIntact`, `writeAuthorSpec` (Task 4), `Manifest.Author` (Task 3), existing `predicateFn` signature.
- Produces: `authorPredicate` registered under `"author"`; `score` treats the author tests gate as absolute; `episode` lands spec files before baselines. Task 6 depends on the episode wiring point.

- [ ] **Step 1: Write the failing test** (append to `bench/bench_test.go`; the predicate's file checks need no engine, so a stub config and temp dir suffice):

```go
func TestAuthorPredicate(t *testing.T) {
	wt := t.TempDir()
	m := Manifest{Kind: "author", Author: &AuthorSpec{Pkg: "example.com/m/echo", Dir: "echo",
		TestFiles: []AuthorFile{{Path: "echo/echo_test.go", Content: "package echo\n"}}}}
	if err := writeAuthorSpec(wt, m.Author); err != nil {
		t.Fatal(err)
	}
	// Spec intact but nothing authored: not yet a pass.
	if ok, _ := authorPredicate(config{}, wt, m, nil); ok {
		t.Fatal("no implementation file must not satisfy the predicate")
	}
	os.WriteFile(filepath.Join(wt, "echo", "echo.go"), []byte("package echo\n"), 0o644)
	if ok, _ := authorPredicate(config{}, wt, m, nil); !ok {
		t.Fatal("spec intact plus implementation present must satisfy the predicate")
	}
	// A gutted spec test fails regardless of the implementation.
	os.WriteFile(filepath.Join(wt, "echo", "echo_test.go"), []byte("package echo // gutted\n"), 0o644)
	if ok, specs := authorPredicate(config{}, wt, m, nil); ok || len(specs) == 0 {
		t.Fatalf("modified spec must fail the predicate with evidence, got ok=%v specs=%v", ok, specs)
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./bench -run TestAuthorPredicate`
Expected: FAIL, `undefined: authorPredicate`.

- [ ] **Step 3: Implement** in `bench/bench_test.go`:

1. The predicate (place with the other predicates):

```go
// authorPredicate: the spec tests are byte-frozen and an implementation
// exists. Behavior is scored by the tests gate (scopedTests already runs
// the new package's directory); the predicate proves the spec was
// honored and something non-test was authored.
func authorPredicate(_ config, wt string, t Manifest, _ map[string]int) (bool, []map[string]any) {
	reason, intact := specFilesIntact(wt, t.Author)
	impl := false
	entries, _ := os.ReadDir(filepath.Join(wt, t.Author.Dir))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			impl = true
			break
		}
	}
	ev := map[string]any{"pkg": t.Author.Pkg, "spec_intact": intact, "impl_present": impl}
	if reason != "" {
		ev["spec_violation"] = reason
	}
	return intact && impl, []map[string]any{ev}
}
```

2. Register it: `"author": authorPredicate,` in the `predicates` map.
3. In `score`, the lazy pristine baseline must not run for author tasks: the parent worktree has no package, its tests cannot run, and a false baseline would make the gate vacuous (`passRule`'s `tests || !baselineTests`). The spec tests ARE the goal, so the gate is absolute. Change:

```go
	if predicate && typecheck {
		tests = scopedTests(c, wt, t)
		if !tests && t.Kind != "author" {
			// Lazy baseline: only a failing gate pays for the pristine
			// worktree that decides whether the gate could pass at all.
			// Author tasks skip it: the spec tests cannot run at the
			// parent, and a vacuous gate would let a non-working package
			// pass (docs/specs/authoring-bench.md, Scoring).
			pw := worktree(b, c, t)
			defer teardown(c, t, pw)
			run(pw, c.cap, "go", "mod", "download")
			baselineTests = scopedTests(c, pw, t)
		}
	}
```

4. In `episode`, land the spec before the warm-up and baseline capture (immediately after the `epDir` MkdirAll block and the request-log setup, before `writeOpencodeConfig`):

```go
	if t.Kind == "author" {
		if err := writeAuthorSpec(wt, t.Author); err != nil {
			b.Fatal(err)
		}
	}
```

Order matters: `baseErrs` is captured after this, so the spec tests' undefined-symbol errors are baseline and only NEW errors count against typecheck.

- [ ] **Step 4: Run the tests**

Run: `go test ./bench -run 'TestAuthorPredicate|TestSpecFreeze'`
Expected: PASS. Then `go vet ./bench`.

- [ ] **Step 5: Commit**

```bash
git add bench/bench_test.go
git commit -m "Author predicate: spec freeze plus authored implementation; tests gate absolute, no vacuous baseline"
```

---

### Task 6: Episode dispatch through the driver (Track A + B join)

**Files:**
- Modify: `bench/bench_test.go` (`episode` near line 279, new `runAgentDriver` beside `runAgent` near line 788)

**Interfaces:**
- Consumes: `ago agent --surface --transcript` (Task 2), `writeAuthorSpec` wiring (Task 5), existing `config` fields (`agoBin`, `endpoint`, `model`, `cap`, `profile`), `agoEnv`, `writeJSON`.
- Produces: author episodes in modes `semantic` and `raw` exec the driver; `agentOut` is the driver's JSONL transcript so `record` lands it as `transcript.jsonl`.

- [ ] **Step 1: Implement `runAgentDriver`** (no practical unit test at this layer: it execs a binary against a live endpoint; the driver loop itself is covered by `internal/agent`'s scripted-fake tests, and the wiring is verified by the smoke round in Task 9):

```go
// runAgentDriver runs one ago agent episode: the author kind's arms both
// live on the first-party driver (ADR 0006), opencode never hosts them.
// The profile lands in the worktree's .ago/agent.json so the sampler
// block rides along, same identity the opencode arms record.
func runAgentDriver(ctx context.Context, c config, wt, prompt, mode string) (string, error) {
	if err := os.MkdirAll(filepath.Join(wt, ".ago"), 0o755); err != nil {
		return "", err
	}
	writeJSON(filepath.Join(wt, ".ago", "agent.json"), map[string]any{
		"default": c.profile.Name,
		"profiles": []map[string]any{{"name": c.profile.Name, "endpoint": c.endpoint,
			"model": c.model, "sampler": c.profile.Sampler}},
	})
	tr := filepath.Join(wt, ".ago", "driver-transcript.jsonl")
	cmd := exec.CommandContext(ctx, c.agoBin, "agent",
		"--surface", mode, "--profile", c.profile.Name,
		"--transcript", tr, "--cap", c.cap.String(), prompt)
	cmd.Dir = wt
	cmd.Env = agoEnv(c)
	out, err := cmd.CombinedOutput()
	if b, rerr := os.ReadFile(tr); rerr == nil {
		return string(b), err
	}
	return string(out), err
}
```

- [ ] **Step 2: Dispatch by kind in `episode`**. Replace the mode branch:

```go
	if mode == "oracle" {
		agentOut, agentErr = runOracle(c, wt, t)
	} else if t.Kind == "author" {
		ctx, cancel := context.WithTimeout(context.Background(), c.cap)
		agentOut, agentErr = runAgentDriver(ctx, c, wt, t.Prompt, mode)
		timedOut = ctx.Err() == context.DeadlineExceeded
		cancel()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), c.cap)
		agentOut, agentErr = runAgent(ctx, c, wt, t.Prompt)
		timedOut = ctx.Err() == context.DeadlineExceeded
		cancel()
	}
```

and skip the opencode config for author tasks (`writeOpencodeConfig` call site):

```go
	if t.Kind != "author" {
		writeOpencodeConfig(b, c, wt, mode)
	}
```

Leave the `mode == "raw" || mode == "serena"` daemon stop as is: the driver serves its tools in-process, so the daemon is irrelevant to author arms either way, and scoring respawns it.

- [ ] **Step 3: Compile and run the bench package's fast tests**

Run: `go vet ./bench && go test ./bench -run 'TestAuthor|TestSpec|TestHasSpecs'`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add bench/bench_test.go
git commit -m "Author episodes ride the first-party driver: profile lands in the worktree, transcript is the evidence, opencode config skipped"
```

Known gaps, file as beads in Task 10, do not fix here: driver episodes record zero `tokens_in`/`tokens_out` (`sumTokens` parses opencode JSON) and zero request counters (the in-process engine does not write the daemon request log).

---

### Task 7: Oracle replay for author (Track B)

**Files:**
- Modify: `bench/author.go`, `bench/bench_test.go` (`runOracle` switch near line 403)
- Test: `bench/author_test.go`

**Interfaces:**
- Consumes: `AuthorSpec` (Task 3), `agoJSONStdin(c, wt, stdin, "patch", "--body-file", "-")`, patch body shape `{"pkg": ..., "ops": [...]}` with ops `{"op": "upsert_decl", "pkg": ..., "text": ...}` (see `bench/compose.go:385`).
- Produces: `authorOracleOps(pkg string, files map[string]string) ([]map[string]any, error)` and the `"author"` case in `runOracle`.

- [ ] **Step 1: Write the failing test** (append to `bench/author_test.go`):

```go
func TestAuthorOracleOps(t *testing.T) {
	files := map[string]string{
		"echo/echo.go": "package echo\n\nimport \"fmt\"\n\n// Echo repeats s.\nfunc Echo(s string) string { return fmt.Sprint(s) }\n\nconst limit = 10\n",
		"echo/aux.go":  "package echo\n\nfunc aux() {}\n",
	}
	ops, err := authorOracleOps("example.com/m/echo", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("want 3 upsert ops (import blocks are the engine's job), got %d: %v", len(ops), ops)
	}
	for _, op := range ops {
		if op["op"] != "upsert_decl" || op["pkg"] != "example.com/m/echo" {
			t.Fatalf("op shape drifted: %v", op)
		}
	}
	// File order is sorted, so aux comes first; the Echo decl carries its
	// doc comment (Rename and set_doc conventions depend on docs moving
	// with decls).
	if !strings.Contains(ops[1]["text"].(string), "// Echo repeats s.") {
		t.Fatalf("doc comment must ride with the decl: %v", ops[1]["text"])
	}
}
```

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./bench -run TestAuthorOracleOps`
Expected: FAIL, `undefined: authorOracleOps`.

- [ ] **Step 3: Implement** in `bench/author.go` (imports grow `go/ast`, `go/parser`, `go/token`, `maps`, `slices`, `strings`):

```go
// authorOracleOps parses ground-truth implementation files into one
// atomic patch of upsert_decl ops, sorted file order then source order.
// One patch, not sequential upserts: the package typechecks whole, so
// intra-package declaration order never rejects. Import blocks are
// skipped; the engine manages imports (set_body/upsert goimports sugar).
func authorOracleOps(pkg string, files map[string]string) ([]map[string]any, error) {
	var ops []map[string]any
	fset := token.NewFileSet()
	for _, path := range slices.Sorted(maps.Keys(files)) {
		src := files[path]
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				continue
			}
			start := d.Pos()
			switch n := d.(type) {
			case *ast.FuncDecl:
				if n.Doc != nil {
					start = n.Doc.Pos()
				}
			case *ast.GenDecl:
				if n.Doc != nil {
					start = n.Doc.Pos()
				}
			}
			text := src[fset.Position(start).Offset:fset.Position(d.End()).Offset]
			ops = append(ops, map[string]any{"op": "upsert_decl", "pkg": pkg, "text": text})
		}
	}
	return ops, nil
}
```

- [ ] **Step 4: Run it**

Run: `go test ./bench -run TestAuthorOracleOps`
Expected: PASS.

- [ ] **Step 5: Wire the oracle case** in `runOracle` (`bench/bench_test.go`), after the `"move"` case, following the existing `call`/transcript pattern:

```go
	case "author":
		repo := filepath.Join(c.scratch, t.Repo)
		out, err := exec.Command("git", "-C", repo, "diff-tree",
			"--no-commit-id", "--name-only", "-r", t.SHA).Output()
		if err != nil {
			return b.String(), err
		}
		files := map[string]string{}
		for f := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if !strings.HasPrefix(f, t.Author.Dir+"/") || !strings.HasSuffix(f, ".go") ||
				strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := exec.Command("git", "-C", repo, "show", t.SHA+":"+f).Output()
			if err != nil {
				return b.String(), err
			}
			files[f] = string(src)
		}
		ops, err := authorOracleOps(t.Author.Pkg, files)
		if err != nil {
			return b.String(), err
		}
		body, _ := json.Marshal(map[string]any{"pkg": t.Author.Pkg, "ops": ops})
		res := agoJSONStdin(c, wt, string(body), "patch", "--body-file", "-")
		rec, _ := json.Marshal(map[string]any{"call": []string{"patch", "author"}, "res": res})
		b.Write(rec)
		b.WriteByte('\n')
		if res["status"] != "accepted" {
			return b.String(), fmt.Errorf("author oracle patch: %v", res)
		}
```

- [ ] **Step 6: Compile, run fast tests, commit**

Run: `go vet ./bench && go test ./bench -run 'TestAuthor'`

```bash
git add bench/author.go bench/author_test.go bench/bench_test.go
git commit -m "Author oracle: ground-truth decls replay as one atomic upsert_decl patch, docs ride with decls, imports left to the engine"
```

---

### Task 8: Extractor prep-author with the coverage gate (Track B)

**Files:**
- Create: `cmd/bench/author.go`
- Test: `cmd/bench/author_test.go` (create)
- Modify: `cmd/bench/main.go` (usage string and dispatch switch)

**Interfaces:**
- Consumes: `bench.Manifest`, `bench.AuthorSpec`, `bench.AuthorFile` (Task 3; check how existing extractors reference the bench package, `cmd/bench/addparam.go` is the model, and mirror its import or local-struct pattern exactly).
- Produces: `bench prep-author -scratch <clones> -out bench/tasks-author.json -min-cover 90 -max-impl-files 1 -max-impl-lines 150`, plus pure helpers `authorShapeOf(paths []string) (authorShape, bool)`, `parseCoverage(out string) (float64, bool)`, `authorPrompt(pkg, dir string, tests []string) string`.

- [ ] **Step 1: Write the failing tests** (`cmd/bench/author_test.go`):

```go
package main

import "testing"

func TestAuthorShapeOf(t *testing.T) {
	shape, ok := authorShapeOf([]string{"libs/echo/echo.go", "libs/echo/echo_test.go"})
	if !ok || shape.Dir != "libs/echo" || len(shape.Impls) != 1 || len(shape.Tests) != 1 {
		t.Fatalf("canonical shape rejected: %+v %v", shape, ok)
	}
	// Two directories is not an author task.
	if _, ok := authorShapeOf([]string{"a/x.go", "b/x_test.go"}); ok {
		t.Fatal("multi-directory commit must not match")
	}
	// Tests without an implementation, or the reverse, is not one either.
	if _, ok := authorShapeOf([]string{"a/x_test.go"}); ok {
		t.Fatal("test-only commit must not match")
	}
	if _, ok := authorShapeOf([]string{"a/x.go"}); ok {
		t.Fatal("untested package must not match: the tests are the spec")
	}
	// Module file edits are the tier 1 ceiling, rejected with the blocker named.
	if _, ok := authorShapeOf([]string{"a/x.go", "a/x_test.go", "go.mod"}); ok {
		t.Fatal("go.mod edits are outside tier 1")
	}
	// Non-Go files in the same new dir (a README) are tolerated and ignored.
	shape, ok = authorShapeOf([]string{"a/x.go", "a/x_test.go", "a/README.md"})
	if !ok || len(shape.Impls) != 1 {
		t.Fatalf("non-Go passengers must not disqualify: %+v %v", shape, ok)
	}
}

func TestParseCoverage(t *testing.T) {
	got, ok := parseCoverage("ok  \texample.com/m/echo\t0.01s\tcoverage: 87.5% of statements\n")
	if !ok || got != 87.5 {
		t.Fatalf("coverage misparsed: %v %v", got, ok)
	}
	if _, ok := parseCoverage("ok example.com/m 0.01s\n"); ok {
		t.Fatal("no coverage figure must not parse")
	}
}
```

- [ ] **Step 2: Run them, watch them fail**

Run: `go test ./cmd/bench -run 'TestAuthorShapeOf|TestParseCoverage'`
Expected: FAIL, undefined symbols.

- [ ] **Step 3: Implement the pure helpers** (`cmd/bench/author.go`):

```go
package main

// prep-author mines greenfield commits structurally: no subject regex,
// the shape is the classifier (docs/specs/authoring-bench.md, Mining).
// Every gate that rejects names its blocker; a silent skip hides the
// roster gap the spec calls out.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type authorShape struct {
	Dir   string
	Tests []string
	Impls []string
}

// authorShapeOf classifies one commit's changed paths: all Go files in a
// single directory, at least one test and one implementation, no module
// file edits. Non-Go passengers (README, configs) ride along ignored.
func authorShapeOf(paths []string) (authorShape, bool) {
	var s authorShape
	for _, p := range paths {
		if p == "" {
			continue
		}
		base := filepath.Base(p)
		if base == "go.mod" || base == "go.sum" {
			return authorShape{}, false
		}
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		dir := filepath.Dir(p)
		if s.Dir == "" {
			s.Dir = dir
		} else if s.Dir != dir {
			return authorShape{}, false
		}
		if strings.HasSuffix(p, "_test.go") {
			s.Tests = append(s.Tests, p)
		} else {
			s.Impls = append(s.Impls, p)
		}
	}
	if s.Dir == "" || len(s.Tests) == 0 || len(s.Impls) == 0 {
		return authorShape{}, false
	}
	return s, true
}

var coverageRe = regexp.MustCompile(`coverage: (\d+(?:\.\d+)?)% of statements`)

func parseCoverage(out string) (float64, bool) {
	m := coverageRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func authorPrompt(pkg, dir string, tests []string) string {
	return fmt.Sprintf("Author the Go package %s. Its tests are already in place (%s); implement the package in %s so they pass. Do not modify the test files.",
		pkg, strings.Join(tests, ", "), dir)
}
```

- [ ] **Step 4: Run the helper tests**

Run: `go test ./cmd/bench -run 'TestAuthorShapeOf|TestParseCoverage'`
Expected: PASS.

- [ ] **Step 5: Implement the walk and dispatch.** In `cmd/bench/author.go`, `prepAuthor(scratch, outFile string, minCover float64, maxImplFiles, maxImplLines int) error`:

1. For each repo clone in `scratch` (same directory-listing approach the existing prep flow uses; see how `main.go` drives the other preps): `git log --no-merges --format=%H%x00%s`, then per commit `git diff-tree --no-commit-id --name-only -r <sha>` and `authorShapeOf`.
2. Reject and continue, logging the blocker to stderr (`fmt.Fprintf(os.Stderr, "skip %s %.8s: %s\n", repo, sha, why)`), on: shape mismatch (silent, the common case, no log), `len(shape.Impls) > maxImplFiles`, total impl lines over `maxImplLines` (count via `git show sha:path`), directory existed at parent (`git ls-tree <sha>^ -- <dir>` output non-empty), package import path underivable (module path from `go show sha:go.mod` fails to parse; derive as `modulePath + "/" + dir`).
3. Coverage gate, the expensive check, runs last and only on survivors: `git worktree add <tmp> <sha>`, `go mod download`, `go test -cover ./<dir>` with a timeout, `parseCoverage`; below `minCover` logs `skip ...: coverage 62.5 below 90` and continues. Always `git worktree remove --force <tmp>`.
4. Survivors become manifests: `Kind: "author"`, `Prompt: authorPrompt(...)`, `GoFiles: len(shape.Impls) + len(shape.Tests)`, `Author: &AuthorSpec{Pkg, Dir, Coverage, TestFiles}` with test contents from `git show <sha>:<path>`. Marshal to `-out` exactly the way `mine` in `cmd/bench/mine.go:79-90` writes its file.
5. `cmd/bench/main.go`: add `prep-author` to the usage string and the dispatch switch with flags `-min-cover` (default `90`), `-max-impl-files` (default `1`), `-max-impl-lines` (default `150`); `-scratch` and `-out` already exist (default `-out` for this verb: `bench/tasks-author.json`).

- [ ] **Step 6: Compile and full-package test**

Run: `go vet ./cmd/bench && go test ./cmd/bench`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/bench/author.go cmd/bench/author_test.go cmd/bench/main.go
git commit -m "prep-author mines greenfield commits: structural shape, tier 1 ceilings, measured coverage gate, blockers named"
```

---

### Task 9: Kind documentation guard and doc rows

**Files:**
- Test: `bench/guards_test.go`
- Modify: `docs/specs/bench.md`, `docs/specs/authoring-bench.md`, `CLAUDE.md` (bench command block)

**Interfaces:**
- Consumes: the `predicates` map (Task 5).
- Produces: a registry-to-doc guard; every scored kind's backticked name must appear in a spec doc.

- [ ] **Step 1: Write the failing guard** (append to `bench/guards_test.go`; it fails now because no doc contains `` `author` `` as a scored kind marker):

```go
// Every scored kind is documented where its scoring is specified; the
// backticked name is the marker, prose mentions do not count. New kinds
// add the predicate entry AND the doc row; this guard makes forgetting
// the row a test failure, not a review catch.
func TestScoredKindsDocumented(t *testing.T) {
	docs := []string{"../docs/specs/bench.md", "../docs/specs/authoring-bench.md"}
	blob := ""
	for _, d := range docs {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Fatal(err)
		}
		blob += string(b)
	}
	for kind := range predicates {
		if kind == "" {
			continue
		}
		if !strings.Contains(blob, "`"+kind+"`") {
			t.Fatalf("kind %q is scored but no spec doc names it in backticks; add a row to docs/specs/bench.md or docs/specs/authoring-bench.md", kind)
		}
	}
}
```

(`guards_test.go` imports grow `os` and `strings`.)

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./bench -run TestScoredKindsDocumented`
Expected: FAIL naming whichever kinds lack backticked doc mentions (at minimum `author`; if `rename`/`add-param`/`move` also fail, add their backticks in the same edit).

- [ ] **Step 3: Fix the docs**

1. `docs/specs/bench.md`, Tasks section: name the shipped kinds in backticks (`` `rename` ``, `` `add-param` ``, `` `move` ``) where they are listed, and add one line: "The `author` kind (greenfield authoring, tests as the spec) is specified in `authoring-bench.md`."
2. `docs/specs/authoring-bench.md`: in the "Task kind" heading's first paragraph, name the kind in backticks: "the `author` kind".
3. `CLAUDE.md` bench block gains the mining line:

```bash
go run ./cmd/bench prep-author -scratch ~/.cache/ago-bench/clones   # mine greenfield author tasks
```

- [ ] **Step 4: Run the guard and the repo's doc guards**

Run: `go test ./bench -run TestScoredKindsDocumented && go test ./cmd/ago -run 'TestReadme|Guard'`
Expected: PASS; any repo guard failure names its file, fix it here.

- [ ] **Step 5: Commit**

```bash
git add bench/guards_test.go docs/specs/bench.md docs/specs/authoring-bench.md CLAUDE.md
git commit -m "Guard scored kinds against the spec docs; author rows land in bench.md and CLAUDE.md"
```

---

### Task 10: Mine, certify, smoke (execution, not code)

**Files:**
- Create: `bench/tasks-author.json` (mined output, committed)
- Create: `bench/results/<run>/...` (oracle evidence, committed)

- [ ] **Step 1: Full gate first**

Run: `gofmt -l . && go test ./...`
Expected: gofmt silent, suite green (~90s for snapshot).

- [ ] **Step 2: Mine**

```bash
go run ./cmd/bench prep-author -scratch ~/.cache/ago-bench/clones -out bench/tasks-author.json
```

Read the stderr skip log. **STOP POINT:** if zero tasks survive, do not synthesize tasks and do not lower the gates silently; report the yield and the skip reasons to Guy. The spec names the fallback (add one small repo to the roster) and that is Guy's call. If tasks survive, eyeball the smallest one's manifest: prompt sane, test contents complete, coverage recorded.

- [ ] **Step 3: Oracle sweep the author tasks**

```bash
AGO_BENCH_MODES=oracle AGO_BENCH_SCRATCH=~/.cache/ago-bench/clones \
  go test ./bench -run 'OracleSweep' -parallel 8 -timeout 0 -v 2>&1 | grep -E 'author|PASS|FAIL'
```

An author oracle rejection is a finding, not a harness bug: either the extractor mis-shaped the task or the protocol has a gap. Record it as a bead either way; a protocol gap found here is the dogfooding working (ADR 0005).

- [ ] **Step 4: Certify and commit evidence**

```bash
go run ./cmd/bench certify bench/results/<run> bench/tasks-author.json
git add bench/tasks-author.json bench/results/<run>
git commit -m "Author tasks mined and oracle certified: <n> tasks, coverage gated, evidence attached"
```

(Match `certify`'s actual argument order to `cmd/bench/certify.go`; flip nothing by hand.)

- [ ] **Step 5: Smoke round, both arms** (needs a serving endpoint from `bench/profiles.json`; **STOP POINT:** if no endpoint is reachable, report and stop rather than faking it):

```bash
AGO_BENCH_PROFILE=<name> AGO_BENCH_MODES=semantic,raw AGO_BENCH_SUITE=smoke \
  AGO_BENCH_SCRATCH=~/.cache/ago-bench/clones \
  go test ./bench -bench Rename -benchtime 1x -timeout 0
```

Verify through the real path: the smoke episode's `episode.json` carries `kind: author`, a driver transcript in `transcript.jsonl`, the spec freeze evidence in `specs`, and mode/profile identity. Commit the run.

- [ ] **Step 6: File the follow-up beads**

```bash
bd create --title="Driver episodes: token accounting in bench evidence" --description="Author episodes record zero tokens_in/tokens_out: sumTokens parses opencode JSON and the driver transcript carries no usage. Surface usage from the OpenAI-compatible response through internal/agent and into episode.json." --type=task --priority=2
bd create --title="Driver episodes: request counters parity" --description="Author episodes have empty request counters: the driver serves tools in-process and does not write the daemon request log AGO_LOG_REQUESTS names. Either log from the in-process path or derive rejects/repairs/resends from the driver transcript." --type=task --priority=2
```

- [ ] **Step 7: Session close**

```bash
git pull --rebase && bd dolt push; git push && git status
```

Expected: up to date with origin. Close `agent-go-zc5` with the evidence run named.

---

## Self-review notes

- Spec coverage: task kind (3, 4, 5), arms on the driver (1, 2, 6), scoring with absolute tests gate (5), oracle and certification (7, 10), mining with coverage gate (8, 10), guards (2 surface test, 4 freeze, 9 kind docs), tier 2 and differential scoring already filed as `agent-go-83k` and `agent-go-obw`.
- Known deferrals, named: driver token accounting and request-counter parity (Task 6, beads in Task 10); roster yield is a stop point, not a silent gate change.
- Type consistency: `AuthorSpec`/`AuthorFile` (Task 3) are consumed by exact shape in 4, 5, 7, 8; `specFilesIntact(wt, spec) (string, bool)` used in 4 and 5; `agentToolDefs(surface string)` in 2 only; `runAgentDriver` consumes Task 2's flags by exact name.
