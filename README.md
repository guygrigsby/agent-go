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
ago search -s MaxEntries    # name fragment -> exact symbol addresses
ago refs -p <pkg> -s <sym>  # every reference, tests included
ago rename -p <pkg> -s DefaultLimiterMaxEntries -to DefaultLimiterMaxQuotas
echo 'return v << 1' | ago set-body -p <pkg> -s Double -body-file -
ago add-param -p <pkg> -s NewLimiter -name ctx -type context.Context -default 'context.Background()'
ago upsert -p <pkg> -body-file decl.go   # add/replace a whole declaration
```

Agents connect over MCP (`ago mcp`); `ago init` writes the wiring.

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

- `docs/specs/protocol.md` — protocol semantics, guarantees, op catalog
- `docs/specs/bench.md` — bench design
- `docs/specs/plan.md` — status and build order
- `docs/adr/` — architecture decisions
- `idea.md` — the original thesis

Experimental. Interfaces change without notice.
