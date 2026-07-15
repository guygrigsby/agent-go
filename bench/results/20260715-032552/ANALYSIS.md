# Rename round 1 (valid): GLM 4.7 Flash, raw vs semantic

Run 20260715-032552. 9 oracle-verified rename tasks, k=1, 20m cap,
GLM-4.7-Flash (UD-Q4_K_XL) on llama.cpp, opencode both modes. Two earlier
rounds were discarded: round 1's data was invalidated by workspace-wide
mutation preflight locking semantic mode out of a rotted-parent task, round
2's by prompts that never named the rename target. Both fixes are in the
history; every episode of every round is under bench/results/.

Scoring: goal predicate + typecheck (no new errors) + scoped tests, with
the tests criterion waived for three tasks whose test suites fail on the
pristine parent (env-dependent: docker/postgres). rejudged.json holds the
final per-episode scores.

## Results

| metric | raw | semantic |
|---|---|---|
| pass | 6/9 | 5/9 |
| median time-to-green (passes) | 635s | 138s |
| episodes leaving a broken tree | 2 | 0 |

Head-to-head where both passed: semantic 3.4-5.1x faster
(06d69f69: 87s vs 333s; 95b24f55: 138s vs 468s; caf19f86: 159s vs 818s).

Semantic-only passes: 9f2c83f3 (14-symbol family rename, 689s; raw gave up
incomplete at 302s) and caec65a7 (SealInfo -> SealWrapper, 30 refs across
7 files, 115s; raw spent the full 20m and left 3 dangling references that
do not compile).

Raw-only passes: 51491463, c0532428, 9237d6f7 — semantic hit the cap on
all three. Those transcripts are the protocol's to-do list.

## Reading

1. The correctness asymmetry is structural, not statistical: semantic mode
   cannot leave a broken tree (validation refuses the write), raw mode did
   so twice in nine tasks. On repo-scale changes that is the difference
   between an agent you can leave alone and one you babysit.
2. When the semantic path fits the task, it is 3-5x faster and uses a
   fraction of the tokens (a one-symbol rename is two tool calls).
3. Completion rate is not yet won: 5/9 vs 6/9. The capped semantic
   episodes need transcript autopsies before the next round; suspects are
   rejection loops and the model failing to map prompt names to symbol
   addresses despite search.
4. k=1 everywhere; treat every number as an estimate. The k=3 round and
   benchstat come once the capped-episode causes are fixed.
