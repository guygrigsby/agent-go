# Engineering tenets

The principles this repo lives by. Each names its enforcement; a tenet
without a mechanism is a wish. The rules are strict on purpose: this work
is meant to be checked, used, and built on by other people, and the rigor
is for them. We care about the work and the humans it lands on.

## 1. Automate everything so drift is impossible

Every fact that lives in two places gets a test comparing them, code side
as the source of truth. Review only has to catch prose semantics;
existence, status, counts, shapes, and examples are machine-checked.
Enforced by the guard lattice (op registry ↔ help catalog ↔ surface.md ↔
language.md ↔ issue status ↔ README ↔ MCP wire ↔ init scaffold ↔ bench
counters), catalog-version hashing, and fixture-executed examples. Adding
a fact without a guard is the bug.

## 2. Prove the task is solvable first

Before any model attempts a benchmark task, replay the known-correct
change (mined from the real commit) through the protocol. If a perfect
agent can't complete the task that way, it doesn't enter the bench; the
blocker gets named instead. Every one of those is a finding (a spec bug,
an engine bug, or a protocol ceiling) caught at machine speed, and no
model time is spent on impossible work. Enforced by TestOracleSweep and
certified-task gating of rounds.

## 3. Rejections are data

Every rejection answers "what is the correct next call": diagnostics say
what broke and `possible_repairs` carries the corrected call whole.
Sending the same rejected call again escalates instead of looping. A
repair that would itself reject is worse than none, so every repair runs
verbatim in tests before it is ever offered. Enforced by the repairs
suite.

## 4. Parallel by default, deterministic by test

Concurrency is the default for independent work (bench episodes,
packages, subagents); serial needs a named reason. Identical query,
identical bytes: caches, benchmarks, and result comparability all hang
on it, and map iteration order is not an ordering. Enforced by the race
detector, the serial/parallel equivalence test, and the byte-equality
determinism test.

## 5. Name the ceiling, ship the floor

Deliberate simplifications are documented where they bind, with the
trigger for lifting them. "v1 ceiling: X, rejected with the blocker
named" is a feature; a silent wrong answer is the bug. Enforced by
ceiling notes in the catalog and specs, and by rejects that name what
blocked them.

## 6. Tolerate the world, reject the change

Real codebases carry history, and the people working in them shouldn't be
blocked by problems they inherited. A mutation is judged only on the
diagnostics it introduces; what was already failing never stops unrelated
work. Same rule shapes scoring: a test gate no ground truth could pass is
vacuous, not failed. Enforced by baseline capture and filter in the
engine and the pristine-worktree baseline in bench scoring.

## 7. Meet the codebase where it is

Production code bends to deadlines, migrations, and business reality.
That is not a defect to punish; it is the medium. The protocol serves the
engineer shipping under those constraints, not an imagined ideal repo,
and nothing here demands a clean world before it helps. Enforced by the
bench itself: every task is mined from a real commit in a production
repo ([traefik](https://github.com/traefik/traefik),
[vault](https://github.com/hashicorp/vault),
[boundary](https://github.com/hashicorp/boundary)), never hand-written,
so the tool is measured on the code people actually live in.

## 8. The data is the contribution

Every run is captured as if a reader will check the work: transcripts,
configs, diffs, scores, tokens, failure kinds, and request logs in git,
serving setups pinned per run, prompts token-counted. A number without
its evidence trail is an anecdote, and nobody has measured
Go-with-local-models in this flavor; the data matters as much as the
code. Enforced by the bench record path, the results-in-git convention,
and the MLflow export.

## 9. Tooling speaks the project's language

No sidecar scripts. A committed script is a missing subcommand; mining,
reporting, extraction, and validation are Go code with tests, living
next to what they serve.
