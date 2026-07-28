# 6. Authoring bench arm on the first-party driver

Status: Accepted (2026-07-28)

## Context

ADR 0005 built the authoring driver with a closed tool surface and
deliberately kept it out of the bench: "the driver is an authoring
tool, not a fourth bench arm, until a deliberate decision says
otherwise." This is that decision. The branch can now author from
empty modules, and the question the project actually cares about is
whether the protocol helps small local models produce working code
from scratch. Answering it through opencode would reintroduce the
host-leak confound 0005 documented: five invalid on-disk states in
the frozen qwen round, suspected host edit-tool leakage.

## Decision

- New bench task kind `author`: greenfield tasks mined from real
  commits that introduce one new package plus its tests, scored by
  the commit's own tests (tests-as-spec, tier 1). Design in
  `docs/specs/authoring-bench.md`.
- Both arms of the authoring comparison run on `ago agent`. Semantic
  serves the shipped surface; raw serves ungated `write_file` with no
  ops. Same loop, same prompts, same profile; only the tool surface
  differs.
- The raw gate is a bench-only mode of the driver, off in normal use;
  a driver test asserts which tools each arm serves.
- The edit bench keeps riding opencode for comparability with
  recorded rounds. Nothing recorded is rerun or rewritten.

## Consequences

- Supersedes the not-a-bench-arm consequence of ADR 0005 for
  authoring kinds only; 0005 stands otherwise.
- The protocol comparison for authoring is confound-free: any invalid
  intermediate state in a semantic episode is an engine bug, not a
  host leak.
- Authoring results are not comparable to the opencode edit rounds
  and are never pooled with them; they are a separate table with
  their own evidence.
- Owning both arms means owning raw-mode prompt fairness: the raw arm
  must be a genuine best effort, not a strawman, or the comparison is
  worthless.
