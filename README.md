# agent-go

`ago` is a semantic edit protocol for Go: agents query the typechecked
workspace and submit compiler-checked mutations instead of editing text.

Go as a language for agents. The protocol puts the Go toolchain between
the model and the files: an edit that does not typecheck cannot reach
disk. LSP-backed tools have the same compiler diagnostics available and
can still write broken code, because checking is advisory there. Here
invalid edits are unrepresentable, not merely checked.

The thesis under test: weak local models become effective repo-scale Go
editors when raw file editing is replaced by semantic queries and validated
mutations with typed rejections. The bench in this repo measures exactly
that, on tasks mined from real commits in traefik, vault, and boundary.

## Install

```
go install github.com/guygrigsby/agent-go/cmd/ago@latest
```

Or from a clone:

```
go build -o ago ./cmd/ago
```

Requires Go 1.26+. The daemon auto-spawns on first use, one per
workspace, and exits after five minutes idle; there is nothing to start
or configure.

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

Rejections are data, not errors: the agent loop is query, mutate, and on
rejection adjust and retry. Most rejections carry `possible_repairs`,
complete paste-ready calls: the corrected mutation, the re-view that
refreshes handles, the search that locates an undefined identifier.
Resending a rejected call unchanged gets an escalated rejection instead
of the same one forever. Rename also proves every rewritten reference
still resolves to the renamed symbol, so shadowing capture is rejected
even when the compiler is satisfied.

## Bench

`bench/` holds the raw-vs-semantic comparison: same local model, same
harness (opencode), same mined tasks (rename and add-param kinds, from
traefik, vault, boundary, and cobra); one mode gets shell and file
editing, the other gets only the protocol.

The oracle is the bench's third arm: a scripted replay of each task's
ground-truth commit through the protocol itself, no model in the loop.
Every mined task carries the real change its source commit made; the
oracle submits that change as protocol calls and must reach green. A
task enters the roster only after the oracle certifies it, so "the model
failed" is never confused with "the task was impossible". Oracle
rejections are findings (an engine bug, a bad extraction, or a named
protocol ceiling), its wall time is the task's time-to-green floor, and
its accepted transcripts seed the fine-tuning corpus.

Scoring is goal predicate + typecheck + scoped tests under a time cap;
the tests gate counts only where a pristine worktree passes the same
tests. Every episode records its transcript, config, diff, score, token
usage, failure kind, and the daemon's per-request log under
`bench/results/`. Serving setups are pinned as named profiles in
`bench/profiles.json` and embedded in each run's `run.json`.

```
# smoke: one certified task per kind, smallest repo, for first contact
AGO_BENCH_SUITE=smoke AGO_BENCH_PROFILE=<name> AGO_BENCH_SCRATCH=<clones dir> \
go test ./bench -bench Rename -benchtime 1x -timeout 0

# model round, k=3
AGO_BENCH_PROFILE=glm-flash AGO_BENCH_SCRATCH=<clones dir> \
go test ./bench -bench Rename -benchtime 3x -timeout 0

# oracle sweep: no model, episodes run in parallel
AGO_BENCH_MODES=oracle AGO_BENCH_SCRATCH=<clones dir> \
go test ./bench -run OracleSweep -parallel 6 -timeout 0

# mine candidate tasks from any clone; report across runs
go run ./cmd/bench mine -scratch <clones dir> <repo>
go run ./cmd/bench report bench/results/<run> ...
```

## Development

```
go test ./...        # full suite; snapshot tests take ~40s
gofmt -l .           # must be empty
```

Rules of the road:

- TDD: write the failing test first, watch it fail, then implement.
  Every op and repair in the tree landed that way.
- Docs are drift-guarded by tests: the op catalog must match
  `docs/specs/surface.md`, help examples must be accepted against the
  test fixture, and README invocations must match the real CLI dispatch.
  Change behavior and the guard tells you which doc to touch.
- The demo fixture (`internal/snapshot/testdata/demo`) is copied per
  test; never mutate it in place. `demo/lib` is frozen behind a recorded
  hash (`TestDemoLibFixtureFrozen`) because the view-handle tests depend
  on its exact layout. `demo/sig` is the place for new fixture shapes.
- Race-sensitive changes (the parallel retypecheck) should run under
  `go test -race ./internal/snapshot`.

## Docs

- `docs/specs/language.md`: the full op catalog, decl through test ops
- `docs/specs/surface.md`: shipped and foreseen calls, drift-guarded by test
- `docs/specs/protocol.md`: protocol semantics and guarantees
- `docs/specs/bench.md`: bench design
- `docs/specs/plan.md`: status and build order
- `docs/optimizations/`: per-model serving research and the cross-model build list
- `docs/tenets.md`: the engineering principles this repo lives by
- `docs/model-strategies.md`: brainstorm on coding with local models
- `docs/adr/`: architecture decisions
- `idea.md`: the original thesis

Work is tracked in [beads](https://github.com/steveyegge/beads): `bd ready`
for the queue, `bd list` for everything; issues sync through
`.beads/issues.jsonl`.

Experimental. Interfaces change without notice.

Inspired by [zero](https://zerolang.ai), a programming language 
for agents. 
