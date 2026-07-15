# Plan

Sequence Guy set: spike → bench design → build. The bet: prove or kill the
thesis on the cheapest sufficient slice (rename tasks) before widening the
op catalog. Status as of 2026-07-15: language core built (T1-T12); bench
round still in flight.

## Language core: built (2026-07-15)

Shipped: generations + view/handles, atomic multi-op patches (`$N` refs,
`dry_run`), 14 statement ops, 8 composable decl ops (incl. atomic
multi-rename), 4 test ops with canonical skeleton and convention
detection, the full query tier (search/inspect/refs/callers/callees/
implementations/doc), the `test` tool, the `help` catalog, ten-tool MCP
surface. Full spec: `docs/specs/language.md`; exact implemented op list:
`ago help`.

Verified end to end against boundary (533 pkgs, private worktree, see
`docs/specs/language-verification.md`): view returns real handles and
generation; a 3-op statement patch ($refs, real handles) applies
atomically; replaying it against the stale generation rejects with
`stale generation: re-view`; `implementations` finds a real 113-type
interface; `callers` walks 28 real call sites; `test` returns structured
pass/fail including an honest environmental failure (missing postgres)
alongside a clean pure-package run.

Ceilings, each documented not silent: package-granular generations;
`replace_expr` limited to condition/whole-expression slots; `add_for` has
no init/post clause; non-comparable test results rejected;
callers/callees are static-only, no method-set candidate expansion.
Deferred (language.md's Future section plus the explicitly named ops):
`move_decl`, `set_signature`, `remove_param`, `implement_interface`,
`add_bench`, all project ops (`add_dependency` etc.), `possible_repairs`
rejection suggestions, SSA query tier, multi-module/go.work,
structured-expression mode, batch cross-repo transactions.

## Done

- Spike: go/packages latency numbers killed CLI-per-invocation (ADR 0001)
  and whole-workspace revalidation (ADR 0002).
- Engine: daemon + snapshot, package-level splice, objectpath identity,
  ops status/inspect/refs/set-body/rename with tests; verified on boundary
  (533 pkgs): queries 8-48ms, rename 29 refs in 231ms.
- Bench assets: 59 mined+validated tasks (`bench/tasks.json`); rename
  manifests with ground-truth-confirmed specs (`bench/tasks-rename.json`,
  11 tasks / 49 specs) via `bench prep-rename`; go-test runner
  (`bench/bench_test.go`) with full episode evidence recording.
- Model plumbing: GLM 4.7 Flash at llama.lab.aeryx.ai, tool calling
  verified; opencode drives both modes.
- `ago init` + MCP front end for usability.

## In flight

1. Smoke run: one rename task, both modes, k=1, 10m cap. First data:
   raw capped without completing. Semantic episode pending.
2. Re-smoke with evidence recording, confirm semantic mode exercises the
   MCP tools end to end.
3. Full rename round: 11 tasks x 2 modes, k=1 recorded, then k=3 as time
   allows. Results committed under bench/results/.

## Next builds

Superseded 2026-07-15 by the language core section above; what's left to
build lives in its Deferred list and language.md's Future section. One
item below wasn't a language-build item and stays open:

1. Bench prep extractors for add-param and move kinds, mirroring
   prep-rename's confirm-against-ground-truth approach.

## Gaps found by the oracle pass (2026-07-15)

Executing every manifest spec through ago surfaced, in order of discovery:
pre-existing rot must not block unrelated mutations (fixed: scoped
preflight); test-forked dependency copies must be in the reverse graph or
splices leave stale type identities (fixed); generated test-main packages
must be ignored by references (fixed); _test.go symbols need test-variant
lookup (fixed); atomic multi-rename, where renaming an interface method
requires renaming its implementations in the same validated transaction,
is fixed by the language core's decl-op patches (T8). One remains open
and blocks one bench task:

- Multi-module repos: vault's sdk/ is a nested module; the engine loads one
  module. go.work-style multi-root loading is the fix.

## Open questions

- Cap: is 10m right? Raw capping is signal, not noise, but the cap must
  not decide the result; watch the time-to-green curve and set the cap
  from it.
- Prompt parity between modes (one system sentence each today).
- Local/alias addressing scheme (scope path or position qualifier).
- go.work expansion (`use` directives) when a bench repo needs it.
