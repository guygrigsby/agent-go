# agent-go


Go as a language for agents. `ago` puts a semantic protocol over the Go
toolchain so a coding agent queries the workspace and submits
compiler-checked mutations instead of editing text.

The thesis under test: weak local models become effective repo-scale Go
editors when raw file editing is replaced by semantic queries and validated
mutations with typed rejections. The bench in this repo measures exactly
that, on tasks mined from real commits in traefik, vault, and boundary.

## How it works

One binary, three fronts over a per-workspace daemon (auto-spawned on first
use, unix socket, idle exit):

```
ago init myproject          # scaffold: module, MCP wiring, AGENTS.md
ago status                  # load the snapshot: packages, files, errors
ago help                    # versioned op catalog: args, one example, notes per op
ago search -s MaxEntries    # name fragment -> exact symbol addresses
ago refs -p <pkg> -s <sym>  # every reference, tests included
ago query -kind implementations -p <pkg> -s <iface>  # interface -> implementing types
ago view -p <pkg> -s <sym>  # declaration as annotated text: node handles + generation
ago rename -p <pkg> -s DefaultLimiterMaxEntries -to DefaultLimiterMaxQuotas
echo 'return v << 1' | ago set-body -p <pkg> -s Double -body-file -
ago add-param -p <pkg> -s NewLimiter -name ctx -type context.Context -default 'context.Background()'
ago upsert -p <pkg> -body-file decl.go   # add/replace a whole declaration
ago patch -body-file patch.json   # ordered, atomic, generation-checked multi-op edit
ago test -p <pkg>                 # go test, scoped, structured pass/fail
```

Agents connect over MCP (`ago mcp`); `ago init` writes the wiring. `patch`
is the full language: 14 statement ops, composable decl ops, table-driven
test ops; `rename`/`set-body`/`add-param`/`upsert` are one-op sugar over
it. Full catalog via `ago help`.

Every mutation validates before anything touches disk: the daemon
re-typechecks only the affected packages against its in-memory graph
(~200ms for a 29-reference rename on a 533-package repo) and rejects with
the compiler's own diagnostics:

```json
{
  "status": "rejected",
  "reason": "edit does not typecheck",
  "diagnostics": [
    {"pos": "config.go:114:29", "msg": "undefined: slices"},
    {"pos": "config.go:9:2", "msg": "\"reflect\" imported and not used"}
  ]
}
```

Rejections are data, not errors: the agent loop is query → mutate →
(rejection → adjust → retry). Rename additionally proves that every
rewritten reference still resolves to the renamed symbol, so shadowing
capture is rejected even when the compiler is satisfied.

## Bench

`bench/` holds the raw-vs-semantic comparison: same local model, same
harness (opencode), same mined tasks; one mode gets shell and file editing,
the other gets only the protocol. Scoring is goal predicate + typecheck +
scoped tests under a time cap; every episode's transcript, config, diff,
and score is recorded under `bench/results/`.

```
AGO_BENCH_ENDPOINT=http://host:port/v1 AGO_BENCH_MODEL=<model> \
AGO_BENCH_SCRATCH=<clones dir> go test ./bench -bench Rename -benchtime 3x -timeout 0
```

## Docs

- `docs/specs/language.md` — the full op catalog: decl, statement, test ops
- `docs/specs/protocol.md` — protocol semantics, guarantees
- `docs/specs/bench.md` — bench design
- `docs/specs/plan.md` — status and build order
- `docs/optimizations/` — per-model serving research and the cross-model build list
- `docs/adr/` — architecture decisions
- `idea.md` — the original thesis

Work is tracked in [beads](https://github.com/steveyegge/beads): `bd ready`
for the queue, `bd list` for everything; issues sync through
`.beads/issues.jsonl`.

Experimental. Interfaces change without notice.

Inspired by [zero](https://zerolang.ai), a programming language 
for agents. 
