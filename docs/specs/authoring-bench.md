# Authoring bench: working code from scratch

The goal is working software from small local models, not measurement
for its own sake. The edit bench asks whether the protocol helps a weak
model change existing code; this arm asks whether it helps one author
code that runs. Scaffolding (tests as the spec, a pinned API) is a
means: start with maximum scaffold so episodes are scoreable, then
remove scaffold tier by tier while holding pass rates. The less a user
must supply up front for the model to succeed, the more useful the
tool. Every tier is still scored the same way: the code works.

## Task kind: author (tier 1, tests as the spec)

A task is mined from a real commit in the roster repos that introduces
one new package plus its tests. Setup per episode:

- Worktree at the parent commit.
- The commit's `_test.go` files are written into the new package
  directory by the harness before the episode starts. Their hashes are
  recorded in the manifest.
- Prompt: the new package's import path plus the instruction to make
  its tests pass. No API description, no file list; the tests carry
  the spec.

The spec tests are frozen. Scoring recomputes their hashes; any change
fails the episode outright, otherwise deleting the test is the shortest
path to green.

## Arms

Both arms run on the first-party driver (`ago agent`), per ADR 0006:

- **semantic**: the shipped surface. Ops, `read_file`, gated
  `write_file` (rejects `.go`), `test`.
- **raw**: `write_file` ungated, `read_file`, `test`, no ops.

Same loop, same prompts, same serving profile; only the tool surface
differs, so the comparison isolates the protocol. The edit bench keeps
riding opencode for comparability with recorded rounds.

## Scoring

An episode passes when all hold within the time cap:

1. Spec test hashes unchanged.
2. `go test` green on the new package.
3. The workspace still builds (typecheck clean, same check the edit
   bench uses).

Evidence identity is unchanged: every episode carries task, mode and
profile, results are append-only, Wilson CIs from `bench report`.

## Oracle and certification

The oracle replays the ground-truth commit's non-test declarations
through the op surface into the fresh package (the empty-module
`upsert_decl` path), then runs the same predicate. A task enters model
rounds only after `bench certify` flips its flag from committed oracle
evidence, the same discipline as the edit kinds. Certification proves
the task is achievable through the protocol before any model time is
spent on it.

## Mining

`cmd/bench mine` grows an author extractor. A candidate commit:

- Touches only files inside one directory that did not exist at the
  parent commit.
- Includes at least one `_test.go` and at least one non-test file.
- Does not touch `go.mod` or `go.sum` (tier 1 ceiling: no new
  dependencies; a task needing one is rejected with the blocker named).
- Is small. Start dead simple, echo-server scale: one non-test file, a
  handful of declarations. Thresholds are tuned by what the roster
  yields, recorded in the extractor, not in prose here.

Open point: vault and traefik class repos may hold nothing that
simple. The fallback is adding one small repo to the roster, never
hand-writing tasks; mined-from-reality is the bench's identity.

## Tier 2: held-out intent (filed, not built)

Prompt from the commit subject, tests applied after the episode. This
is the scaffold-removal axis: tier 1 pins the API through visible
tests, tier 2 asks the model to design it. Scores there conflate
wrong-API with wrong-code, so it is designed only after tier 1 is
green. Tracked as a bead blocked on tier 1.

## Guards

- The author kind lands under the existing lattice: `HasSpecs` arm,
  classify entry, prompt template, kind row here, each with the guard
  the edit kinds already have.
- Spec test hashes in the manifest are a recorded-hash freeze; the
  scoring check doubles as the guard.
- The raw-mode surface (which tools each arm serves) is asserted by a
  driver test, not prose, so this file cannot drift from the actual
  gate.

## Open

- Roster gap for dead-simple greenfield commits (see Mining).
- Multi-file packages: excluded at tier 1, revisit once single-file
  tasks are green.
- Interactive authoring (a human in the loop refining the spec) is out
  of bench scope entirely; the bench stays one-shot.
