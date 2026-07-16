# ago: a semantic edit protocol for Go workspaces

Thesis (from idea.md): weak local models become effective repo-scale Go
editors when restricted to semantic queries and validated mutations instead
of raw text editing. The protocol is the product; the bench proves or kills
the thesis.

## Language

- **Workspace** — one Go module tree (go.work planned). Unit of daemon
  ownership and snapshot state.
- **Snapshot** — the typechecked in-memory view of a workspace: packages,
  syntax, types, reference info. Owned by the daemon, never by clients.
- **Symbol address** — `pkg` (import path) + `sym` (`Name`, or
  `Type.Member` for methods and fields). Locals and import aliases are not
  addressable yet; that needs scope- or position-qualified addressing.
- **Query** — read-only question answered from the snapshot: `status`,
  `search`, `inspect`, `refs`, `callers`, `callees`, `implementations`,
  `doc`. Milliseconds; never touches disk. List results page at 50 with
  a total `count`, `truncated`, and `next_offset`/`offset`.
- **Mutation** — a checked edit: the full op catalog in language.md
  (decl, statement, and test ops, incl. `set_signature` and `move_decl`),
  plus the four sugar tools. Validated against the snapshot before any
  file is written. All-or-nothing: failure writes nothing and rolls back
  everything. An accepted mutation that reshaped exactly one declaration
  embeds that declaration's fresh view in its response.
- **Rejection** — the structured refusal of a mutation: reason, detail,
  compiler diagnostics, `did_you_mean` candidates, and `possible_repairs`
  — complete paste-ready next calls. A rejection is data for the agent,
  not an error; the agent loop is query → mutate → (rejection → repair →
  retry). Exact resends of a rejected call escalate instead of repeating.
- **Splice** — package-level revalidation: re-typecheck only the dirty set
  (edited-file packages, plus transitive reverse importers when the edit
  can change API or method sets) and swap results into the live snapshot.
  Packages check in parallel, one goroutine each, scheduled over the
  post-edit import graph (~56ms for a 117-package dirty set). Identity
  across splices is objectpath-based (ADR 0002).

## Guarantees

1. A mutation never introduces a new compiler diagnostic. Pre-existing
   diagnostics in the affected packages are tolerated (baselined at
   preflight, filtered post-splice, surfaced as `pre_existing`), so
   unrelated rot never blocks an edit.
2. A rejected mutation changes nothing on disk or in the snapshot.
3. Rename additionally proves resolution: every rewritten reference must
   resolve to the renamed object afterward, so shadowing capture is
   rejected even when the compiler is satisfied.
4. Queries reflect all accepted mutations immediately (no reload window).
5. External (non-protocol) edits are detected per request and trigger a
   full reload; correctness over latency in that path.

## Surface

One binary, three fronts over one daemon (unix socket per workspace,
auto-spawned, idle-exit; gopls `-remote=auto` precedent, ADR 0001):

- CLI: `ago init|status|inspect|refs|rename|set-body|stop`, JSON out,
  exit 2 on rejection.
- MCP: `ago mcp` over stdio for agent harnesses; tools `ago_*` mirror the
  CLI ops; rejections returned as payloads, not tool errors.
- `ago init` scaffolds an agent-first project: compilable module, MCP
  wiring, AGENTS.md protocol instructions.

## Op catalog

Implemented surface: the ten-tool MCP/CLI surface (status, help, query,
view, patch, test, rename, set_body, add_param, upsert_decl) plus the
full patch op catalog underneath `patch`. See `docs/specs/language.md`
for the language spec and `ago help` for the versioned, per-op catalog
(args, one example, notes) that always matches what's built.

Non-goals for now: multi-module go.work, non-Go files, formatting choices
(gofmt is the only style), IDE features (hover docs, completion).
