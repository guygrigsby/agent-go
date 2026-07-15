# Qwen optimizations for the ago language

Brainstorm only, nothing here is committed work. Assumes the language spec
(`docs/specs/language.md`) as designed: six tools, patch transactions,
node handles, generations, text-atom expressions with a structured form
designed in. Question addressed: what changes, tunings, or additions make
this language work well specifically on Qwen models served locally.

Scope: local means what an average developer can run; the cap is a 128GB
unified-memory Mac. Everything below fits with room to spare.

Model targets as of 2026-07, in likely order of use:

- Qwen3-Coder-Next (2026-02): 80B total, 3B active (10 of 512 experts),
  hybrid Gated DeltaNet + Gated Attention, 262k native context, ~52GB at
  Q4_K_M, 70.6% SWE-bench Verified. Purpose-built for coding agents and
  local serving; the primary target.
- Qwen3.6-27B, MTP variant preferred: dense, the community-consensus
  single-GPU coder, ~17GB at Q4, 262k native. MTP self-speculation gives
  1.4 to 2.2x generation speedup and llama.cpp support is merged. The 3.6
  line also added a developer role and more robust nested-argument tool
  call parsing, so it drops into OpenCode-class harnesses cleanly.
- Qwen3.6-35B-A3B: the MoE sibling when throughput beats the dense 27B's
  per-token quality.
- Prior generation (Qwen3-Coder-30B-A3B, Qwen2.5-Coder dense line): still
  widely deployed; keep serving profiles for them, but they are not the
  models to optimize for. Everything below that assumed Qwen2.5's 32k
  window is updated in place.

## Serving and decoding

The highest-leverage layer. Most "weak model" failures on tool protocols
are actually serving-stack failures.

- **Tool-call parser must match the family.** Qwen3-Instruct uses
  Hermes-style tool calls; the Coder line (including Coder-Next) uses its
  own XML-ish format needing the dedicated parser (`--tool-call-parser
  qwen3_coder` in vLLM, correct `--jinja` template and a current build in
  llama.cpp). A mismatched parser silently drops or mangles calls and the
  bench will read it as model failure. Verify the parser end to end
  before any Qwen bench round, same as was done for GLM.
- **Published sampling settings, not defaults, and they move per
  generation.** Coder-Next wants temp 1.0, top_p 0.95, top_k 40, a
  complete reversal of the old Coder card (temp 0.7, top_p 0.8,
  repetition_penalty 1.05); thinking variants still forbid greedy
  decoding (greedy loops). The settings belong in the per-model serving
  profile, re-checked against the model card at every family bump, never
  carried forward by habit.
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
  validity and string escaping; Unsloth's Coder-Next guidance names Q6_K
  or higher specifically for tool schema compliance. Either hold that
  floor for bench runs (the 128GB cap leaves plenty of headroom for Q6 on
  every target), or pair lower quants with grammar constraints and let
  the grammar absorb the damage. Record the quant in episode metadata
  either way; otherwise quant noise pollutes the raw-vs-semantic
  comparison.
- **Speculative decoding loves this language, and MTP made it native.**
  Patch JSON is highly predictable text: repeated keys, op names from a
  small vocabulary, handle strings. The Qwen3.6 MTP variants
  self-speculate at 1.4 to 2.2x with no draft model and no accuracy loss,
  and the predictability of patch JSON should push acceptance above those
  headline numbers. Prefer the MTP checkpoint wherever one exists; a tiny
  same-family draft model is the fallback for checkpoints without MTP
  heads. Wall-clock per episode is the metric this feeds, since the bench
  scores time-to-green under a cap.
- **Prefix caching as a design constraint, now with a hybrid-attention
  caveat.** Local prefill dominates episode time. Keep the stable
  material (system prompt, AGENTS.md content, tool schemas, help catalog)
  as an unchanging prefix so the server's prefix cache hits every turn.
  Concretely: deterministic tool ordering in MCP `tools/list`, no
  timestamps or volatile state early in the prompt, and `help` output
  should be byte-stable per catalog version so a cached help call stays
  cached. Caveat: Coder-Next and Qwen3.6 are hybrid stacks (3 of 4 layers
  Gated DeltaNet, linear attention with recurrent state), and prefix
  reuse over recurrent state is engine-dependent in a way pure-KV models
  never were. Verify actual cache-hit behavior on the serving stack
  before counting on this; the compensation is that the few quadratic
  layers carry tiny KV (4 KV heads), so long-context cost dropped even
  when the cache misses.

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

- **Short episodes are still the selling point; the argument changed.**
  The old version of this claim leaned on Qwen2.5's 32k window and YaRN's
  quality tax; current targets are 262k native, so the window is no
  longer the constraint. What survives, and still favors semantic mode:
  prefill wall-clock scales with context regardless of the window, the
  hybrid linear-attention layers make long contexts cheap to hold but do
  not make instruction-following at depth free, and Qwen's adherence to
  tool protocols still degrades as the transcript grows. A few thousand
  tokens of views and query results beats a few hundred thousand of raw
  file reads on time-to-green even when both fit. Say it that way in the
  bench writeup; the YaRN framing is stale.
- **AGENTS.md tuned per family.** `ago init` writes the protocol
  instructions; the Qwen-tuned version should be short, imperative, and
  example-led. One worked loop (view, patch, rejected, repair, accepted),
  one rule per line, no prose paragraphs. Long nuanced system prompts are
  where small Qwen models start ignoring instructions.
- **Thinking mode policy.** The Coder line is non-thinking; this axis
  applies when a thinking-capable general checkpoint plays orchestrator.
  Plausible split: think on the first turn of an episode (plan which
  symbols and ops), plain decode for mechanical follow-ups (re-issue a
  repaired patch, add test cases). Thinking tokens burn the 10m cap fast
  on local hardware; an always-think config can lose on time-to-green
  while winning on first-patch quality. Worth an explicit bench axis:
  Coder-Next vs a thinking generalist vs think-first-turn-only on the
  same tasks.
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
emitting literal Go text and historically weak at multi-step tool
choreography. Coder-Next narrows that gap (it was trained on agentic
trajectories and posts 70%-class SWE-bench numbers), but the question
stands for the dense 27B and everything smaller: the winning
semantic-mode strategy there may be coarse, `view` then
`set_body`/`upsert_decl` with whole compiler-checked bodies, ignoring
the statement vocabulary entirely, while a stronger orchestrator wins
with statement ops on precision and token cost.

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
  the distribution is real refactors, not toy edits. Qwen3.6-27B is the
  natural adapter target (dense, cheap to LoRA, trivially served);
  Coder-Next is the stretch target since MoE fine-tuning is fussier. The
  target skill is protocol fluency, not Go knowledge, so small adapters
  should move the needle.
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

- **FIM as an expression micro-tool.** The Coder line has strong
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

## Sources

Model data as of 2026-07:

- [Qwen3-Coder-Next model card](https://huggingface.co/Qwen/Qwen3-Coder-Next)
  and [technical report](https://arxiv.org/abs/2603.00729): 80B-A3B, 10 of
  512 experts, hybrid attention, 262k native, 70.6% SWE-bench Verified.
- [Unsloth: running Qwen3-Coder-Next locally](https://unsloth.ai/docs/models/qwen3-coder-next):
  temp 1.0 / top_p 0.95 / top_k 40, `qwen3_coder` parser, Q6_K floor for
  tool schema compliance, ~52GB at Q4_K_M.
- [Unsloth: Qwen3.6](https://unsloth.ai/docs/models/qwen3.6) and the
  [Qwen3.6-27B card](https://huggingface.co/Qwen/Qwen3.6-27B): MTP 1.4 to
  2.2x with llama.cpp support merged, developer role, nested tool-arg
  parsing fixes, hybrid Gated DeltaNet layout, ~17GB at Q4.
