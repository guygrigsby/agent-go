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

- ago_status — load the workspace; reports packages, files, type errors
- ago_search {query} — find symbols by name fragment; use this to turn a
  name from a task into exact pkg and sym addresses
- ago_inspect {pkg, sym} — kind, type signature, declaration position
- ago_refs {pkg, sym} — every reference across the workspace, tests included
- ago_rename {pkg, sym, to} — rename at every reference; rejected on
  collision, capture, or type errors, and nothing is written
- ago_set_body {pkg, sym, body} — replace a function body (statements only,
  no braces); rejected with compiler diagnostics if it does not typecheck
- ago_add_param {pkg, sym, name, type, default} — append a parameter and
  update every call site with the default expression; rejected when the
  function is used as a value
- ago_upsert_decl {pkg, text} — add or replace one whole top-level
  declaration from source text; imports managed automatically; new package
  paths under the module are created on demand

sym is Name for package-level symbols, Type.Method for methods and fields.

## Workflow

1. ago_status to see the workspace.
2. ago_search to find symbols, ago_inspect / ago_refs to understand them.
3. Mutate with ago_rename / ago_set_body / ago_add_param / ago_upsert_decl.
4. A rejection is data: read the diagnostics, adjust, retry.

CLI equivalents exist for humans: ago status | inspect | refs | rename |
set-body, plus ago stop to shut down the workspace daemon.
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
