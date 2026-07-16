# Engineering tenets

The principles this repo lives by. Each names its enforcement; a tenet
without a mechanism is a wish.

## 1. Automate everything so drift is impossible

Every fact that lives in two places gets a test comparing them, with the
code side as the source of truth. Prose semantics are the only thing
review has to catch; existence, status, counts, shapes, and examples are
machine-checked. Enforced by the guard lattice: op registry ↔ help
catalog ↔ surface.md ↔ language.md ↔ beads status ↔ README ↔ MCP wire ↔
init scaffold ↔ bench counters, plus catalog-version hashing and
fixture-executed examples. Adding a fact without a guard is the bug.

## 2. Prove it with a failing test first

TDD, strictly: watch the test fail, then implement. A test that never
failed proves nothing. Bugs reproduce in a test before they are fixed.
Enforced by review and by habit; every op, repair, and driver in the
tree landed red-first.

## 3. The oracle goes first

Never spend model time on a task a perfect agent can't complete: replay
ground truth through the protocol, certify or name the blocker. Oracle
rejections are findings — each one is either a spec bug, an engine bug,
or a protocol ceiling, and all three are wins found at machine speed.
Enforced by TestOracleSweep and the certified-task gating of rounds.

## 4. Rejections are conversation, not failure

Every rejection answers "what is the correct next call": diagnostics say
what broke, `possible_repairs` carries the corrected call whole, exact
resends escalate. A repair that would itself reject is worse than none —
so repairs are executed in tests before they are ever offered. Enforced
by the repairs test suite (every repair runs verbatim and must succeed).

## 5. Determinism is a feature with a test

Identical query, identical bytes: prefix caches, benchmarks, and result
comparability all hang on it. Map-iteration order is not an ordering.
Enforced by the byte-equality determinism test and the serial/parallel
equivalence test.

## 6. Parallel by default, serial by proof

Concurrency is the default for independent work — episodes, packages,
subagents. Serial needs a named reason (one endpoint, one GPU, a true
data dependency). Correctness under parallelism is proved, not assumed:
race detector on, equivalence against the serial driver, scheduling
edges from post-edit reality.

## 7. Name the ceiling, ship the floor

Deliberate simplifications are documented where they bind, with the
trigger for lifting them — never silently absorbed. "v1 ceiling: X;
rejected with the blocker named" is a feature; a silent wrong answer is
the bug. Enforced by ceiling notes in the catalog and specs, and by
rejects that name what blocked them.

## 8. Evidence is part of the result

Every bench episode records its transcript, config, diff, score, tokens,
failure kind, and the daemon's request log — committed to the repo.
A number without its evidence trail is an anecdote. Enforced by the
bench runner's record path and the results-in-git convention.

## 9. Tolerate the world, reject the change

Real repos carry rot; pre-existing brokenness never blocks an unrelated
edit, and a mutation is judged only on the diagnostics it introduces.
The same rule shapes scoring: a test gate no ground truth could pass is
vacuous, not failed. Enforced by baseline capture/filter in the engine
and the pristine-worktree baseline in bench scoring.

## 10. Tooling speaks the project's language

No sidecar scripts: a committed script is a missing subcommand. Mining,
reporting, extraction, and validation are Go code with tests, living
next to what they serve.

## 11. The funnel is honest

Candidates → extracted specs → oracle-certified → measured. Every stage
records what it dropped and why (`needs_review`, oracle rejections,
`views_omitted`, truncation markers). Silent shrinkage reads as
coverage; the funnel must say what it lost.
