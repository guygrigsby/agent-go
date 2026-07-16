---
name: ago
description: Use when editing Go code in a repository where the ago binary or its MCP tools are available (ago on PATH, an AGENTS.md mentioning ago, or ago_* MCP tools), especially for cross-file refactors: rename, signature changes, moving declarations, adding parameters. Query the typechecked workspace and submit compiler-checked mutations instead of editing Go files as text.
---

# ago: semantic edits for Go

ago puts the Go toolchain between you and the files: an edit that does
not typecheck cannot reach disk. Prefer it over raw file edits for any
Go change that touches more than one site, and for any change where
being wrong is expensive. Single-file body tweaks are fine either way.

## The loop

1. `ago status` once; the daemon auto-spawns, nothing to configure.
2. Find the target: `ago search -s <fragment>` turns a name fragment
   into exact pkg/sym addresses. `sym` is always a symbol address
   (`Name` or `Type.Member`), never a file name or file:line.
3. Read before writing: `ago inspect` for signature and method sets,
   `ago refs` for every usage, `ago view` for a declaration with node
   handles and its generation.
4. Mutate: `ago rename`, `ago set-body`, `ago add-param`, `ago upsert`
   for single edits; `ago patch --body-file -` for ordered multi-op
   transactions (atomic: all ops land or none do). `ago help` is the
   full op catalog with a lift-ready example per op; trust it over any
   memory of the surface.
5. Behavior check when done: `ago test -p <pkg> --run <filter>`.

## Rejections are the conversation

Exit 2 means rejected, and the JSON payload is instructions, not an
error:

- `possible_repairs` are complete calls. Resend the first one exactly
  as given. Never resend a rejected call unchanged; that escalates.
- `diagnostics` carry the compiler's positions. `did_you_mean` lists
  address candidates.
- `stale generation: re-view` means the declaration changed since your
  view; re-view it and rebuild handles before retrying.
- A rejection changed nothing on disk. There is no cleanup step.

## Rules

- Node handles (`n1`, `n7`) come from `view` and are valid only against
  the generation that view reported.
- Accepted single-declaration mutations embed a fresh view in the
  response; use it instead of re-viewing.
- Do not "fix" a rejection by dropping to raw file edits. The rejection
  names what is actually wrong; a text edit that dodges the check ships
  the bug the check caught.
- MCP hosts see the same surface as `ago_*` tools; the CLI and MCP are
  the same ops.
