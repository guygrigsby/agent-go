# Engineering tenets

The principles this repo lives by. Each names its enforcement; a tenet
without a mechanism is a wish.

## 1. Automate everything so drift is impossible

Every fact that lives in two places gets a test comparing them, code side
as the source of truth. Review only has to catch prose semantics;
existence, status, counts, shapes, and examples are machine-checked.
Enforced by the guard lattice (op registry ↔ help catalog ↔ surface.md ↔
language.md ↔ beads status ↔ README ↔ MCP wire ↔ init scaffold ↔ bench
counters), catalog-version hashing, and fixture-executed examples. Adding
a fact without a guard is the bug.

## 2. The oracle goes first

Never spend model time on a task a perfect agent can't complete: replay
ground truth through the protocol, certify or name the blocker. Every
oracle rejection is a finding (a spec bug, an engine bug, or a protocol
ceiling), all found at machine speed. Enforced by TestOracleSweep and
certified-task gating of rounds.

## 3. Rejections are conversation, not failure

Every rejection answers "what is the correct next call": diagnostics say
what broke, `possible_repairs` carries the corrected call whole, exact
resends escalate. A repair that would itself reject is worse than none,
so every repair runs verbatim in tests before it is ever offered.
Enforced by the repairs suite.

## 4. Parallel by default, deterministic by test

Concurrency is the default for independent work (episodes, packages,
subagents); serial needs a named reason. Identical query, identical
bytes: caches, benchmarks, and result comparability all hang on it, and
map iteration order is not an ordering. Enforced by the race detector,
the serial/parallel equivalence test, and the byte-equality determinism
test.

## 5. Name the ceiling, ship the floor

Deliberate simplifications are documented where they bind, with the
trigger for lifting them. "v1 ceiling: X, rejected with the blocker
named" is a feature; a silent wrong answer is the bug. Enforced by
ceiling notes in the catalog and specs, and by rejects that name what
blocked them.

## 6. Tolerate the world, reject the change

Real repos carry rot. Pre-existing brokenness never blocks an unrelated
edit; a mutation is judged only on the diagnostics it introduces. Same
rule shapes scoring: a test gate no ground truth could pass is vacuous,
not failed. Enforced by baseline capture and filter in the engine and the
pristine-worktree baseline in bench scoring.

## 7. The data is the contribution

Every run is captured as if a reader will check the work: transcripts,
configs, diffs, scores, tokens, failure kinds, and request logs in git,
serving setups pinned per run, prompts token-counted. A number without
its evidence trail is an anecdote, and nobody has measured
Go-with-local-models in this flavor; the data matters as much as the
code. Enforced by the bench record path, the results-in-git convention,
and the MLflow export.

## 8. Tooling speaks the project's language

No sidecar scripts. A committed script is a missing subcommand; mining,
reporting, extraction, and validation are Go code with tests, living
next to what they serve.

## 9. The funnel is honest

Candidates → extracted specs → oracle-certified → measured. Every stage
records what it dropped and why (`needs_review`, oracle rejections,
`views_omitted`, truncation markers). Silent shrinkage reads as
coverage; the funnel says what it lost.
