# Qwen optimizations for the ago language

Brainstorm only, nothing here is committed work. Assumes the language spec
(`docs/specs/language.md`) as designed: six tools, patch transactions,
node handles, generations, text-atom expressions with a structured form
designed in. Question addressed: what changes, tunings, or additions make
this language work well specifically on Qwen models served locally.

Model targets, in likely order of use on lab hardware:

- Qwen3-Coder-30B-A3B (MoE, 3B active): the realistic daily driver, fast
  on a single card, 256k native context.
- Qwen2.5-Coder 7B/14B/32B: dense, 32k native (128k only via YaRN),
  strongest small models at literal Go text, weaker at long agentic loops.
- Qwen3 Instruct/Thinking (2507 split): general models with hybrid or
  dedicated thinking; relevant for the planning half of an episode.

## Serving and decoding

The highest-leverage layer. Most "weak model" failures on tool protocols
are actually serving-stack failures.

- **Tool-call parser must match the family.** Qwen3-Instruct uses
  Hermes-style tool calls; Qwen3-Coder uses its own XML-ish format needing
  the dedicated parser (`qwen3_coder` in vLLM, correct `--jinja` template
  in llama.cpp). A mismatched parser silently drops or mangles calls and
  the bench will read it as model failure. Verify the parser end to end
  before any Qwen bench round, same as was done for GLM.
- **Published sampling settings, not defaults.** Qwen cards are specific:
  Coder wants temp 0.7, top_p 0.8, repetition_penalty 1.05; thinking
  variants want temp 0.6, top_p 0.95 and explicitly no greedy decoding
  (greedy loops). Bake these into the bench harness per model so runs are
  comparable and the repetition failure mode is suppressed at the sampler.
- **Constrained decoding is the payoff the spec already designed for.**
  The structured expression grammar was fixed "so a llama.cpp GBNF grammar
  can force validity at the decoder." Qwen is the family to cash that in
  on: vLLM structured outputs (xgrammar) can enforce a JSON schema per
  tool call, and llama.cpp takes a GBNF for the whole patch envelope. Two
  tiers worth building:
  1. Envelope grammar now: valid patch JSON, known op names, known arg
     keys per op. Kills hallucinated ops and misspelled keys outright, no
     model strength required.
  2. Expression grammar later: a Go expression GBNF for text atoms, or the
     structured node form once implemented. This is the
     "structured-expressions-only mode" future item; Qwen makes it worth
     pulling forward because small quants misplace quotes and parens
     inside expressions more than they misplace JSON structure.
- **Quantization floor.** Q4 quants measurably degrade tool-call JSON
  validity and string escaping. Either hold the line at Q5/Q6 minimum for
  bench runs, or pair Q4 with grammar constraints and let the grammar
  absorb the damage. Record the quant in episode metadata either way;
  otherwise quant noise pollutes the raw-vs-semantic comparison.
- **Speculative decoding loves this language.** Patch JSON is highly
  predictable text: repeated keys, op names from a small vocabulary,
  handle strings. A tiny draft model (Qwen 0.5B/1.5B of the same family)
  should get high acceptance rates on the scaffolding and materially cut
  wall-clock per episode, which matters because the bench scores
  time-to-green under a cap.
- **Prefix caching as a design constraint.** Local prefill dominates
  episode time. Keep the stable material (system prompt, AGENTS.md
  content, tool schemas, help catalog) as an unchanging prefix so
  llama.cpp/vLLM prefix cache hits every turn. Concretely: deterministic
  tool ordering in MCP `tools/list`, no timestamps or volatile state early
  in the prompt, and `help` output should be byte-stable per catalog
  version so a cached help call stays cached.

## Tool surface and schema shape

- **The ten-tool consolidation (Task 12) is the right call for Qwen;
  protect it.** Qwen instruction-following degrades as tool count and
  schema size grow. Keep the per-op schemas out of the MCP tool
  definitions: `patch` advertises `{pkg, sym, generation, ops}` with ops
  as an opaque array, and the op vocabulary lives in `help`. The model
  pulls op detail on demand instead of paying for the full catalog every
  turn.
- **Return the fresh view in the patch accept response.** Today's flow is
  patch, then re-view to get new handles and generation. Every saved
  round trip is a full prefill saved. Accept responses should carry
  `{generation, view}` for the touched declaration so the common
  edit-then-edit-again loop needs no `view` call between patches. Same
  for `rename`/`set_body` sugar.
- **Encourage parallel read-only calls.** Qwen3 emits parallel tool calls
  when the template allows it. Queries are read-only and cheap
  (milliseconds from the snapshot), so the AGENTS.md guidance should say
  outright: batch your `query` calls (refs + callers + inspect in one
  turn). One prefill, three answers.
- **Help examples must be paste-ready.** Qwen leans hard on few-shot
  imitation. Every `help` entry should carry one complete, valid,
  minimal patch JSON that would be accepted against a toy fixture,
  because that is exactly what the model will template from. The Task 12
  test already asserts examples parse as valid patch JSON; extend that to
  "would validate against the fixture" if cheap.

## Prompt and context economy

- **Staying inside the native window is a thesis-level selling point.**
  Raw mode on a big repo wants YaRN-stretched context, and YaRN costs
  short-context quality. Semantic mode keeps episodes in a few thousand
  tokens of views and query results, comfortably inside Qwen2.5-Coder's
  native 32k. State this in the bench writeup and never enable YaRN for
  semantic-mode runs; it is part of the measured advantage.
- **AGENTS.md tuned per family.** `ago init` writes the protocol
  instructions; the Qwen-tuned version should be short, imperative, and
  example-led. One worked loop (view, patch, rejected, repair, accepted),
  one rule per line, no prose paragraphs. Long nuanced system prompts are
  where small Qwen models start ignoring instructions.
- **Thinking mode policy.** Hybrid Qwen3 lets the harness choose per
  turn. Plausible split: `/think` for the first turn of an episode (plan
  which symbols and ops) and `/no_think` for mechanical follow-ups
  (re-issue a repaired patch, add test cases). Thinking tokens burn the
  10m cap fast on local hardware; an always-think config can lose on
  time-to-green while winning on first-patch quality. Worth an explicit
  bench axis: Instruct vs Thinking vs hybrid-scheduled on the same tasks.
- **View compaction.** Views are the bulk of context growth in long
  episodes. Options, cheapest first: cap rendered body length with an
  elision marker and per-statement handles intact; a `view {depth}` arg
  that renders signatures-only for large decls; guidance in AGENTS.md to
  re-view only on stale-generation rejects (the generation check makes
  stale reads safe, so hoarding old views is pure waste). The handle
  format itself is cheap in the Qwen tokenizer (short ASCII lines,
  `nK:` prefixes are one or two tokens); no change needed there.
- **No markdown fences in view or help output.** Qwen mirrors formatting
  it sees. Fenced Go in a view invites the model to answer with fenced Go
  in a JSON string, which is exactly the escaping failure below. Keep
  protocol text plain.

## Failure-mode hardening

Known Qwen-family failure shapes and the engine-side answer to each.

- **String escaping in code-bearing args.** `set_body` and `upsert_decl`
  carry multi-line Go inside JSON strings: newlines, tabs, quotes, `%w`
  verbs. Small models get this wrong constantly, and it is the single
  most likely "semantic mode loses" mechanism. Mitigations, stackable:
  a lenient decode pass on patch payloads (accept literal newlines and
  tabs inside strings, trailing commas, single-quoted keys) with the
  repair noted in the response; steering toward statement ops whose args
  are short single-line atoms (the fine-grained vocabulary exists partly
  for this); grammar-constrained decoding, which prevents the malformed
  JSON from ever being sampled.
- **Rejections as literal next calls.** `possible_repairs` should be
  complete op objects the model can copy into its next patch unchanged,
  not op names with prose. Qwen executes "send exactly this" far more
  reliably than "consider adding a parameter." Same for `did_you_mean`:
  full addresses, ready to substitute.
- **Identical-resubmission detection.** Qwen under pressure re-sends the
  same failing call. The daemon can hash incoming patches and, on an
  exact repeat of a just-rejected patch, escalate the rejection: shorter
  reason, harder imperative ("this exact patch was rejected; view the
  declaration and rebuild against generation N"). Cheap circuit breaker
  for the loop-until-cap failure mode.
- **Hallucinated ops and args.** The `unknown op` reject with catalog
  suggestions (Task 3) covers the recovery path; the envelope grammar
  covers prevention. Both are worth having, since grammar is per-stack
  and the reject works everywhere.
- **Tool-name drift.** Qwen sometimes calls `ago_view` when the tool is
  `view` or invents `get_view`. Either register forgiving aliases at the
  MCP layer or normalize obvious variants server-side and note the
  normalization in the response.

## Op granularity: let the bench pick the tier per model

The thesis says fine-grained validated ops help weak models. Qwen-Coder
cuts against it in one specific way: these models are unusually strong at
emitting literal Go text and unusually weak at multi-step tool
choreography. It is plausible that for Qwen2.5-Coder-14B the winning
semantic-mode strategy is coarse: `view`, then `set_body`/`upsert_decl`
with whole compiler-checked bodies, ignoring the statement vocabulary
entirely, while for a stronger orchestrator the statement ops win on
precision and token cost.

So: record op-mix per episode in the bench JSONL (already recording
evidence; add op names and per-op accept/reject), and consider a third
bench arm, semantic-coarse (tool surface restricted to view/query/
set_body/upsert_decl/rename), against semantic-full. If coarse wins on
small Qwen, that is not a thesis failure, it is a routing table: op tier
by model capability, and AGENTS.md per tier.

## Training path: make ago native to Qwen

The oracle harness is a training-data generator hiding in a test suite.
It already executes ground-truth-derived patches for every bench task.

- **SFT from oracle traces.** Render each oracle solution as a full
  episode transcript (status, queries, view, patch, accept) in Qwen chat
  format with tool calls. Mined tasks span traefik, vault, boundary, so
  the distribution is real refactors, not toy edits. LoRA on
  Qwen3-Coder-30B-A3B or Qwen2.5-Coder-14B; the target skill is protocol
  fluency, not Go knowledge, so small adapters should move the needle.
- **Preference pairs from bench episodes.** Every recorded episode
  yields (rejected patch, repaired accepted patch) pairs against
  identical context: natural DPO data for teaching repair behavior and
  killing the resubmission loop.
- **Rejection-recovery SFT.** Synthesize episodes that begin mid-failure
  (stale generation, unknown handle, typecheck reject) and end with the
  correct recovery call. Recovery is the behavior weak models lack most,
  and it is cheap to generate mechanically by perturbing oracle patches.

This is the long game: the language spec is versioned and stable enough
to be a training target, which is an advantage raw-mode editing can never
have (there is no "raw editing catalog" to fine-tune against).

## Further out

- **FIM as an expression micro-tool.** Qwen2.5-Coder has strong
  fill-in-middle. An engine-side option: when an expression atom fails to
  parse or typecheck, ask a small base Coder model to FIM the slot given
  the surrounding declaration, and offer the result in
  `possible_repairs`. Keeps the big model on orchestration, uses the
  small one where it is genuinely best.
- **Logit-bias nudges** where grammar is unavailable: bias against
  backtick and fence tokens inside tool-call argument decoding.
- **Per-model serving profiles in the bench harness**: parser, template,
  sampler, quant, grammar on/off as one named profile per model, recorded
  in episode metadata, so a Qwen result is reproducible and the
  serving-stack variables are never confounded with the protocol
  variables.

## Measurement before any of it

Add to the episode JSONL, cheap and immediately: tool-call parse failure
count, JSON decode failure count (and whether lenient repair fixed it),
op mix with per-op accept/reject, identical-resubmission count, tokens in
and out per turn, time-to-first-accepted-mutation. Every optimization
above is a hypothesis; these six counters are what say which ones are
worth building for Qwen specifically.
