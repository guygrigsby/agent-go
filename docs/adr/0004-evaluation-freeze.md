# 4. Evaluation freeze protocol

Status: Accepted (2026-07-17)

## Context

Every bench run to date has fed a development loop: rejections became
repairs, oracle failures became engine fixes (import cycles, fork-universe
type identity, grouped members). That loop is a contribution, but results
produced while the engine chased the tasks cannot also be the paper's
headline evidence. Two mined vault commits edit vendored copies of
another module; the workspace model deliberately addresses main-module
packages only. The raw-vs-semantic comparison also conflates symbol
addressing with mandatory validation; an arm with addressing but only
advisory validation separates the two variables.

## Decision

- All runs before the freeze tag are development data. The
  bench-driven-fixes loop is documented methodology, auditable via the
  ago_rev stamped on every run.
- When the grid is ready, tag a release; only frozen-tag runs feed the
  paper's headline tables.
- Serena (LSP symbol addressing, advisory validation) is a frozen-grid
  arm, not a side ablation: the grid is 3 models x raw, serena, semantic
  (plus effort cells). agent-go-uj8 gates the tag.
- Landing before the tag, because frozen episodes must carry them:
  invalid-intermediate rate metric (agent-go-pna), the
  pass-with-oracle_reject recording fix (agent-go-kxa).
- The dev task set freezes at the certified 21. vault_45b0179a and
  vault_7ec1fe75 are skipped by name: their commits edit vendored or
  nested-module code, which v1 does not address (the paper names this
  ceiling; agent-go-6yh tracks the capability post-paper).
- Held-out tasks (etcd, caddy, hugo candidates) are mined now but
  oracle-certified only AFTER the tag, with the frozen engine, so no
  held-out task can influence a fix. Certification rate on unseen repos
  is itself reported. Frozen grid runs on whatever certifies; dev and
  held-out results appear side by side.
- Profiles as pinned: qwen3.5-9b runs the udq4 profile in the freeze;
  the Q4_K_M round remains a development datapoint.

## Consequences

- Post-tag engine changes fork the evidence: anything discovered after
  the tag lands in a new dev phase toward a future tag, never in the
  frozen tables.
- The tag is gated on agent-go-uj8, agent-go-pna, agent-go-kxa; nothing
  else blocks it.
- Skipped vendor-era tasks cap the move roster at 3 certified until
  more move tasks are extracted or agent-go-6yh lands.

## Amendment (2026-07-17, same day)

Closing agent-go-kxa surfaced that cobra_0960ff7f was certified on
vacuous evidence: a pre-modules commit mined pkg "" specs, every count
compared zeros, and certify had no validity gate. The task is revoked
(bench certify now validates specs and revokes on failing or unverifiable
evidence; predicates refuse zero-baseline specs). The dev set freezes at
the certified 20, not 21.
