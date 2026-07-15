# ago on DeepSeek V4 via ds4

Brainstorm: optimizations specific to running the ago language against
DeepSeek V4 Flash/Pro served locally by antirez's ds4 (DwarfStar). No
implementation here; candidate levers, ranked by expected payoff, each
with the ds4 mechanism it exploits. Nothing in this doc changes the
protocol's semantics; it tunes the surface and the harness for one
serving stack.

## The economics that drive everything

ds4 on an M-class box: prefill 460-650 t/s at long context, generation
21-34 t/s, cached prefix free. Generation is 15-20x more expensive than
prefill, and cache hits cost nothing. So the design pressure is:

1. Shift tokens from the model's output to the prompt. Verbose tool
   results are nearly free; verbose tool calls are not.
2. Keep every byte of the conversation prefix stable so ds4's KV
   checkpoint keeps hitting.
3. Make the model's output as predictable as possible: predictable
   tokens are what greedy decoding, MTP drafts, and 2-bit quants get
   right.

ago's shape is already on the right side of this trade. Raw mode makes
the model emit whole file bodies (a 500 token file is 20s of generation
before the first compile-repair loop); semantic mode emits op JSON, 50-150
tokens a call. The levers below widen that gap.

## KV cache alignment (highest payoff, mostly harness work)

ds4 keeps a single mutable KV checkpoint keyed by SHA1 of the rendered
byte prefix, chunk-aligned at 2048 tokens, spillable to disk
(`--kv-disk-dir`). For stateless OpenAI-style clients it maps tool-call
ids back to the exact sampled DSML bytes (a radix tree,
`--tool-memory-max-ids`) so resent histories stay byte-identical and the
checkpoint survives. Everything that breaks byte identity re-prefills the
whole conversation.

- **Byte-stable tool results.** The daemon already returns
  `map[string]any` through `encoding/json` (sorted keys, deterministic).
  Keep that property as a stated invariant: no timestamps, no map
  iteration order leaks, no counters that change on re-query of an
  unchanged snapshot. A response that differs between identical calls is
  a cache killer two turns later.
- **Harness must not reflow tool results.** Exact DSML replay only works
  if the client resends results byte-for-byte. Verify opencode does not
  truncate, re-indent, or re-marshal `ago` results; if it does, that is
  a harness bug worth fixing before any tuning.
- **Path hygiene in bench prompts.** k=3 episodes can share the cold
  checkpoint (system prompt + tool catalog) only if no per-episode
  scratch path appears in the prefix. Worktree paths belong in daemon
  state, never in the prompt. Same for task ids.
- **Version-stable catalog.** `help` being versioned already implies
  this: the tool schemas rendered into the system prompt must be
  byte-identical across a bench run. Regenerating the catalog with a
  different field order between episodes silently costs a full prefill
  per episode.
- **One session per workspace.** ds4 holds one mutable checkpoint;
  interleaving two agents on one server thrashes it. Bench workers get
  a server each, or run serially. Matches the daemon's
  one-socket-per-workspace shape; the pairing (daemon session, ds4 disk
  session) resumes together after a restart.

## Emission-side design: make output copyable, flat, and greedy

- **Repairs as paste-ready ops.** `possible_repairs` should carry
  literal op JSON, not prose. For a weak model, copying beats composing;
  for ds4, copied spans are exactly what MTP drafts accept and greedy
  decoding reproduces. The rejection channel becomes the constrained
  decoder we don't otherwise have. Same for `did_you_mean`: return the
  corrected address in the exact field syntax the next call needs.
- **Flat tools over nested payloads for the common case.** ds4 forces
  temperature 0 on DSML structure but samples argument payloads at the
  request's settings, and one deeply nested `ops` array is a single
  giant argument. The sugar ops (`rename`, `set_body`, `add_param`,
  `upsert_decl`) sidestep that; consider promoting more high-frequency
  ops to flat single-op tools. Tool catalog size is prefix-cached (paid
  once), emission tokens are paid every call, so the usual "keep the
  tool list small" instinct inverts here. Keep `patch` for genuinely
  atomic multi-op transactions.
- **Greedy for ops, thinking for plans.** Patch emission wants
  temperature 0 end to end. Thinking mode ignores client sampling
  entirely, so the split is by mode, not by knob: planning turns (which
  symbols, which ops) may use thinking; emission turns use the
  non-thinking alias (`deepseek-chat`) with greedy settings. Worth a
  bench axis: Flash with thinking on vs off per mode. Suspicion:
  thinking helps raw mode (it has to plan around missing structure) and
  buys semantic mode little, which would itself be a thesis point.
- **MTP.** Experimental in ds4 and currently a slight speedup at best,
  but op JSON is the best case for it: fixed keys, handle refs, short
  values. Free to enable (`--mtp`, greedy only); measure, don't assume.

## No grammar support: lean on the protocol, not the decoder

ds4 has no GBNF/JSON-schema constrained decoding. The spec's structured
expression form targeted a llama.cpp grammar; on ds4 that lands
differently:

- Text atoms stay the primary expression form for ds4. The structured
  form loses its main consumer until ds4 grows a grammar sampler.
  Upstream contribution is plausible (llama.cpp lineage) but is someone
  else's roadmap; don't block on it.
- DSML itself is half the win: the XML-ish format with the `|DSML|`
  token exists because JSON-in-string tool calls kept breaking on
  escaping, and ds4 pins the structural tokens to temperature 0. Tool
  call syntax validity is near guaranteed; only argument content can be
  wrong, and argument content is exactly what ago validates.
- Rejection retries are cheap by construction: the failed turn's prefix
  is cached, so a retry costs only the rejection prefill (small) plus
  regenerated ops. Validation-and-repair at the protocol layer is the
  constrained decoder, priced at one round trip.

## Quantization as a bench axis, not just a deployment detail

ds4 ships q2-imatrix, q2-q4-imatrix, q4-imatrix for Flash (Pro variants
above that), with checkpoints reusable across quants when prefixes
match. 2-bit routed-expert quantization degrades exact-token recall
(identifiers, paths, long literals) faster than short structured
choices. That is aimed straight at the thesis: raw mode leans on exact
recall, semantic mode replaces it with `search`, `did_you_mean`, and
handles. Running the same task set across three quant levels of one
model on one box gives the pass-rate-vs-model-strength curve almost
free, and shared prefill cache across quants makes it cheaper still.
Expectation to test: semantic mode's margin grows as bits drop.

## Context policy: the 1M window is a trap locally

Sparse attention (CSA plus the lightning indexer) makes million-token
context work, and ds4 streams KV to SSD. None of that changes the
arithmetic: 200k tokens of prefix is 5-6 minutes of prefill on a cache
miss, against a single mutable checkpoint. The protocol is already the
context compressor (a `refs` list instead of grepped file bodies); keep
episodes small instead of spending the window.

- Bound every list-shaped response: refs and callers capped with a total
  count and an explicit truncation marker so the model can page rather
  than receive 400 sites.
- Views stay per-declaration, never per-file. Already the design; worth
  stating as a ds4-motivated invariant.
- Long-context modes (Think Max triggers only past model-card context)
  should never engage on a well-behaved episode. If they do, the episode
  already went wrong.

## Harness options

opencode against ds4-server's `/v1/chat/completions` works today and
exact DSML replay makes the stateless client safe. Two upgrades worth
considering later:

- **ds4-agent as a native harness.** In-process inference, session is
  the on-disk KV cache, tools handled without DSML conversion, KV
  mismatch impossible by construction. It is alpha and in C, so porting
  the ago tool surface in is real work; the cheap first step is
  measuring opencode-vs-native overhead on a few episodes to see if the
  work is worth it.
- **Session resume across bench runs.** `/save`, `/switch`, and disk KV
  mean a warm system-prompt checkpoint can outlive the server. Bench
  setup can pre-warm the catalog prefix once per run.

## Priority order

| lever | cost | expected payoff |
|---|---|---|
| repairs/did_you_mean as literal op JSON | already spec'd, keep it strict | high: turns retries into copies |
| byte-stable results + harness replay audit | small, mostly verification | high: keeps every turn cache-hit |
| path/task-id hygiene in bench prompts | trivial | high for bench throughput |
| thinking on/off as a bench axis | config | medium: may be a thesis point |
| flat single-op tools for hot ops | small surface addition | medium: cuts nested-arg failures |
| quant sweep (q2, q2-q4, q4) | bench time only | medium: strengthens the claim |
| response caps + pagination | small | medium: protects prefill budget |
| MTP enablement | config | low but free |
| ds4-agent native port | large | unknown; measure first |

## Open questions

- Does opencode preserve tool-result bytes verbatim on resend? Exact
  replay depends on it and nothing in our stack verifies it today.
- Does DSML emit multiple tool calls per turn, and does ds4's replay
  handle that? Batched queries would save round trips if so.
- Is there upstream appetite in ds4 for a grammar sampler? The
  structured expression form is designed and waiting on exactly that.

## References

- [ds4 (DwarfStar)](https://github.com/antirez/ds4)
- [DeepSeek-V4: Towards Highly Efficient Million-Token Context Intelligence](https://arxiv.org/html/2606.19348v1)
- [DeepSeek-V4: a million-token context that agents can actually use](https://huggingface.co/blog/deepseekv4)
- [DeepSeek V4 Review](https://medium.com/@leucopsis/deepseek-v4-review-a23ce940151c)
- [ktransformers DeepSeek-V4-Flash notes](https://github.com/kvcache-ai/ktransformers/blob/main/doc/en/DeepSeek-V4-Flash.md)
