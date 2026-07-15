# Plan

Sequence Guy set: spike → bench design → build. The bet: prove or kill the
thesis on the cheapest sufficient slice (rename tasks) before widening the
op catalog. Status as of 2026-07-14 night.

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

## Next builds (order fixed by bench task counts and the greenfield gap)

1. ~~`add_param`~~ done: callers updated with explicit default, value uses
   rejected with positions, verified on boundary (2 cross-package callers,
   75 pkgs rechecked, 1.6s).
2. ~~`upsert_decl`~~ done: whole-declaration add/replace with goimports
   in the loop, agent.go landing spot, new packages created under the
   module on demand.
3. Rejection upgrades: `possible_repairs` naming the ops that would fix
   each rejection (idea.md's original design).
4. `move_decl` (24 tasks): hardest op; only after the rename+add_param
   rounds have produced results.
5. Bench prep extractors for add-param and move kinds, mirroring
   prep-rename's confirm-against-ground-truth approach.

## Gaps found by the oracle pass (2026-07-15)

Executing every manifest spec through ago surfaced, in order of discovery:
pre-existing rot must not block unrelated mutations (fixed: scoped
preflight); test-forked dependency copies must be in the reverse graph or
splices leave stale type identities (fixed); generated test-main packages
must be ignored by references (fixed); _test.go symbols need test-variant
lookup (fixed). Two remain open and each blocks one bench task:

- Multi-module repos: vault's sdk/ is a nested module; the engine loads one
  module. go.work-style multi-root loading is the fix.
- Atomic multi-rename: renaming an interface method requires renaming its
  implementations in the same validated transaction; one-at-a-time rename
  correctly rejects the intermediate broken states. A batch op fixes this
  and matches zerolang's multi-op patches.

## Open questions

- Cap: is 10m right? Raw capping is signal, not noise, but the cap must
  not decide the result; watch the time-to-green curve and set the cap
  from it.
- Prompt parity between modes (one system sentence each today).
- Local/alias addressing scheme (scope path or position qualifier).
- go.work expansion (`use` directives) when a bench repo needs it.
