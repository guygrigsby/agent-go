# Round 20260715-223716: glm-flash, k=4, 11 oracle-certified tasks

Semantic 32/44 (73%) vs raw 28/44 (64%). Excluding vault_cfff8d42 (below):
semantic 32/40 (80%) vs raw 24/40 (60%). Median time-to-green lower for
semantic on 8 of 9 tasks where both modes pass. Capped episodes: raw 21,
semantic 13.

Semantic wins 6 tasks (largest: caec65a7, the 30-reference vault seal
rename, 4/4 at 393s median vs raw 2/4 at cap; 9f2c83f3 and traefik both
4/4 vs 2/4). Raw wins 2: c0532428 (4/4 vs 3/4) and cfff8d42.

## The cfff8d42 anomaly (raw 4/4, semantic 0/4)

Oracle-certified add-param task: add ctx to backend.instanceIamRoleARN,
an unexported method on an unexported type. GLM never found the method
address: 66 rejections across the logged episodes, 40 of them
method-or-field-not-found, including `backend.func_1` — an invented
LSP-style closure name — resent 30 times through breaker escalation.
Only 4 mutation attempts total. Raw mode greps the string and wins in
~306s.

Two gaps filed with this evidence: inspect-on-a-type-lists-its-methods
(the missing discovery move) and a redirect repair for synthetic funcN
names. Also observed: 38 repairs offered, effectively zero uptake —
glm-flash ignores possible_repairs even under escalation, in contrast
to the qwen3.5-9b smoke where recovery worked unaided. Repair uptake is
now a first-class cross-model comparison axis.

## Notes

- k=4 (benchtime 3x plus the harness sizing iteration).
- This round's binary predates the episode-counter merge; resends and
  repair counts come from the per-episode requests.jsonl, not
  episodes.jsonl.
- 9237d6f7 semantic: 3 scored_fail (finished, failed predicate) — the
  one task where semantic completes but edits the wrong thing; worth a
  look alongside the next round.
