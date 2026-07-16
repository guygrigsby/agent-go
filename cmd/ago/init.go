package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// runInit scaffolds an agent-first Go project: a module that typechecks,
// MCP wiring so agent harnesses find the ago tools, and an AGENTS.md that
// teaches the protocol. The daemon needs no setup; it auto-spawns on the
// first query.
func runInit(dir, module string) error {
	if module == "" {
		module = filepath.Base(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return fmt.Errorf("go.mod already exists in %s", dir)
	}
	cmd := exec.Command("go", "mod", "init", module)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod init: %v\n%s", err, out)
	}

	files := map[string]string{
		// The snapshot needs at least one package; agents need at least one
		// function to mutate.
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("hello from ` + module + `")
}
`,
		".mcp.json": `{
  "mcpServers": {
    "ago": {
      "command": "ago",
      "args": ["mcp"]
    }
  }
}
`,
		"opencode.json": `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "ago": {
      "type": "local",
      "command": ["ago", "mcp"],
      "enabled": true
    }
  }
}
`,
		"AGENTS.md": `# Working in this repository

This project is edited through ago, a semantic protocol over the Go
toolchain. Query the workspace instead of grepping; submit checked
mutations instead of editing text. Every mutation is validated by the
compiler before anything is written; a rejection tells you exactly why.

## Tools (MCP server "ago")

Ten tools total. sym is Name for package-level symbols, Type.Method for
methods and fields.

- ago_status — load or refresh the workspace; reports packages, files,
  type errors. No arguments.
- ago_help — the full versioned op catalog: every patch op's argument
  schema, one worked example, and its known ceilings. Call this before
  using an op you haven't used yet.
- ago_query {kind, pkg, sym, q} — semantic questions, dispatched by kind:
  search (name fragment -> exact pkg/sym addresses, fragment in q),
  inspect (kind, signature, decl position), refs (every reference, tests
  included), callers / callees (static call-graph edges), implementations
  (interface <-> concrete type, both directions), doc (doc comment).
- ago_view {pkg, sym} — render a declaration as text. Functions and
  methods get a per-statement nK: handle on each line plus a generation
  counter; other declarations render as plain source.
- ago_patch {pkg, sym, generation, dry_run, ops} — apply an ordered list of
  ops as one atomic, generation-checked transaction: every op applies to
  an in-memory copy, the dirty set re-typechecks once, then everything
  writes together or nothing does. An op can reference a handle an earlier
  op in the same list returned, as $1, $2, ... (1-based op index).
  dry_run:true previews the outcome without writing. A decl or test op
  (rename, set_body, add_param, upsert_decl, delete_decl, set_doc,
  add_field, remove_field, add_test, add_test_case, set_test_case,
  remove_test_case) and a statement op cannot edit the same file in one
  patch; run them as separate patches. ago_help lists every op; the four
  below are also reachable standalone, each as sugar for a one-op patch.
- ago_test {pkg, run} — run go test, scoped to a package and optionally
  filtered by name; structured pass/fail, elapsed time, failure output.
  Validation of mutations stays compiler-only — this is how you close the
  behavior loop after a set of changes.
- ago_rename {pkg, sym, to} — rename at every reference; rejected on
  collision, capture, or type errors, and nothing is written.
- ago_set_body {pkg, sym, body} — replace a function body (statements only,
  no braces); rejected with compiler diagnostics if it does not typecheck.
- ago_add_param {pkg, sym, name, type, default} — append a parameter and
  update every call site with the default expression; rejected when the
  function is used as a value.
- ago_upsert_decl {pkg, text} — add or replace one whole top-level
  declaration from source text; imports managed automatically; new package
  paths under the module are created on demand.

## A worked patch: two ops, one $1 reference

ago_view {"pkg": "demo/lib", "sym": "Store.Put"} first, to read the current
generation and get handles (nK:) for the statements to address. Then:

    ago_patch {
      "pkg": "demo/lib", "sym": "Store.Put", "generation": 14,
      "ops": [
        {"op": "add_if", "at": "n1", "where": "before", "cond": "v < 0"},
        {"op": "add_return", "at": "$1", "where": "first", "exprs": ["ErrNegative"]}
      ]
    }

op 1 inserts an empty "if v < 0 { }" before handle n1 and binds its
then-block to $1; op 2 places a return statement first inside that block
by addressing $1 instead of guessing the handle a re-view would assign.
Both ops apply, retypecheck, and write as one unit, or neither does.

## Workflow

1. ago_status to see the workspace (auto-spawns the daemon on first call).
2. ago_query kind=search to turn a task's names into exact pkg/sym
   addresses; kind=inspect / refs to understand them.
3. ago_view the target to get handles and its current generation.
4. Compose an ago_patch against those handles and that generation — or use
   a sugar tool (ago_rename / ago_set_body / ago_add_param /
   ago_upsert_decl) for a single well-known edit.
5. A rejection is data. When it carries possible_repairs, send the first
   repair's call exactly as given — it is a complete, corrected call, not
   a hint. Otherwise read the diagnostics and did_you_mean, adjust, retry.
   A "stale generation" rejection means re-view before retrying.
6. ago_test to confirm behavior once the edits typecheck — typechecking is
   not the same as correct.

CLI equivalents exist for humans: ago status | help | query | view | patch
| test | rename | set-body | add-param | upsert, plus ago stop to shut
down the workspace daemon.
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("initialized %s (module %s)\n", dir, module)
	return nil
}
