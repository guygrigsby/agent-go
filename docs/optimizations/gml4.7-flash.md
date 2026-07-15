# ago on GLM-4.7-Flash via llama.cpp

Brainstorm: optimizations specific to running the ago language against
GLM-4.7-Flash, the current bench model (llama.lab.aeryx.ai, R9700, served
by llama.cpp's llama-server behind an OpenAI-compatible endpoint). Same
framing as deepseek.md: candidate levers only, no protocol changes, ranked
at the end. Where deepseek.md and this doc disagree, it is the serving
stack disagreeing, not the protocol.

## What this model actually is

31.2B total parameters, ~3B active per token (30B-A3B MoE), MLA
attention, 202,752 max context, interleaved thinking (reasons before
each tool call rather than planning upfront), XML-ish `<tool_call>`
format parsed by the `glm47` parser. Z.ai's tool-calling sampling
recommendation: temperature 0.7, top-p 1.0, min-p 0.01, repeat penalty
disabled. 3B active params cuts both ways: fast decode for its class,
weak exact recall. Weak exact recall is the population ago is built
for; this model is the thesis's home turf.

## Constrained decoding is real here (the ds4 story inverted)

llama.cpp has GBNF, a json_schema to GBNF converter used by
llama-server's `response_format`, and lazy grammars triggered by tool
tokens under `--jinja`. The spec's structured expression form and the
one-grammar-covers-a-whole-patch idea have their intended consumer on
this stack. Two hard caveats before leaning on it:

- **The GLM-4.7-Flash grammar bug.** llama.cpp issue 19068: with
  `--jinja` tool definitions the server intermittently enters a
  corrupted state (grammar stuck "awaiting trigger", RAM climbing 1.1GB
  to 9.5GB, gibberish from the first token) and the health check keeps
  returning OK. Closed not-planned. Reported trigger correlate:
  consecutive user messages without an assistant turn between them.
- **The scoring_func bug.** llama.cpp shipped GLM-4.7-Flash with
  `softmax` where the model wants `sigmoid` expert scoring; fixed
  Jan 21, and before the fix it caused looping and poor output. Any
  bench binary older than that fix measures a broken model.

Consequences:

- Pin the llama.cpp build in the bench manifest like a model hash.
  Results are not comparable across server versions for this model.
- The bench needs a per-episode canary, because a corrupted server
  passes health checks: a fixed probe completion before each episode,
  checked against its known-good output, with server restart on
  mismatch. Without it one corruption event poisons a run and reads as
  model failure, in either mode.
- Adopt constrained decoding incrementally: `json_schema` per sugar
  tool first (small schemas, well-trodden path), the full patch grammar
  only after the canary infrastructure exists. Text atoms remain the
  fallback the moment grammar mode looks implicated in a wedge.
- Harness rule: never emit consecutive user messages. Tool results must
  ride the tool-result role, not a second user turn. Worth an explicit
  check in the opencode config rather than an assumption.

## Sampling policy for op emission

- Z.ai's tool-calling numbers (0.7 / 1.0 / 0.01) are the default;
  bench an alternative arm at temperature 0 for patch-emission turns.
  With grammar constraints on, low temperature costs little diversity
  where it matters (expression atoms) and buys determinism everywhere
  else.
- **Repeat penalty stays off.** Op JSON is intentionally repetitive:
  `"op"`, `"at"`, `"where"` keys every element, handle refs reused
  across ops. A repetition penalty taxes exactly the tokens the
  protocol needs repeated and is a plausible silent degrader of
  multi-op patches. The recommendation is already "disabled or 1.0";
  make it a pinned bench invariant, not a default inherited by luck.
- Interleaved thinking means the model reasons before every tool call.
  That is the right shape for query-then-patch loops, but each thought
  is generation-priced. Bench axis: thinking on vs off (template kwarg
  or /nothink) per mode. Same suspicion as deepseek.md: raw mode needs
  the thinking more than semantic mode does, and showing that is itself
  a result.

## KV cache alignment, llama-server flavor

llama-server caches per slot with `cache_prompt` (default on), reuses
the longest matching token prefix, and `--cache-reuse` enables chunk
reuse past the first divergence. No exact-replay machinery like ds4:
the resent conversation is re-rendered through the jinja template and
re-tokenized, so prefix hits depend on the client resending
byte-identical history and the template being deterministic.

- Byte-stable daemon responses (sorted keys, no timestamps, no
  nondeterministic ordering) carry over from deepseek.md unchanged, and
  matter here with no replay fallback to save us.
- One bench worker per slot, `--parallel N` sized to match; slots split
  the KV budget, and a worker hopping slots loses its prefix. With MLA
  the KV is small enough that a few long-context slots fit where dense
  models would not.
- `--slot-save-path` plus the slots API can persist a warmed
  system-prompt cache across server restarts; cheap to wire into bench
  setup, same payoff as ds4's disk sessions.
- Set `--cache-reuse` so a retry after a rejection (same prefix, new
  tail) reuses everything up to the divergence instead of re-prefilling
  the episode.

## MoE and quantization

- UD-Q4_K_XL runs in ~18GB, comfortable on the R9700 with context to
  spare; UD-Q2_K_XL exists below it. Same quant-sweep argument as
  deepseek.md: pass rate vs bits on one box, expecting semantic mode's
  margin to widen as bits drop. With only ~3B active params the model
  is already at the weak end; Q2 on top of A3B is the harshest
  affordable test of the thesis.
- Speculative decoding buys little here: decode is already fast for the
  quality class (3B active), and there is no official small GLM draft.
  Skip unless generation profiling says otherwise.

## Context policy

202k max, MLA makes it affordable, and the advice does not change:
the protocol is the context compressor, keep episodes small, cap
list-shaped responses with counts and truncation markers. One
GLM-specific note: interleaved thinking accumulates reasoning blocks in
the transcript, and harnesses differ on whether they resend them.
Resending grows the prefix (cache-friendly but token-hungry); stripping
them breaks prefix match at every turn boundary. Find out which
opencode does and pick deliberately; stripping plus `--cache-reuse` is
probably the right pair.

## Priority order

| lever | cost | expected payoff |
|---|---|---|
| pin llama.cpp build post scoring_func fix | trivial | bench validity, not speed |
| per-episode canary + restart-on-gibberish | small script | high: one corruption event otherwise poisons a run |
| repeat penalty pinned off, tool-call sampling pinned | config | high: silent JSON degrader removed |
| no consecutive user messages, verified in harness | small audit | high: sidesteps the known wedge trigger |
| json_schema on sugar tools | small | medium-high: first real constrained decoding win |
| --cache-reuse + slot-per-worker + slot save | config | medium: cheap retries, warm starts |
| thinking on/off bench axis | config | medium: likely thesis point |
| quant sweep Q4 vs Q2 | bench time | medium |
| full patch GBNF grammar | moderate | high ceiling, gated on canary infra |
| speculative decoding | config | low, probably skip |

## Open questions

- Does opencode resend reasoning blocks and tool results byte-identical?
  Determines whether prefix caching works at all past turn one.
- Is the issue-19068 wedge reachable with grammars off and tools on?
  Decides whether the canary is needed even before constrained decoding
  lands.
- Does the full-patch grammar fit llama.cpp's GBNF converter limits
  (recursive expression nodes, op-indexed $N refs), or does the
  structured form need a flattened v1?

## References

- [GLM-4.7-Flash on Hugging Face](https://huggingface.co/zai-org/GLM-4.7-Flash)
- [Zhipu AI releases GLM-4.7-Flash (MarkTechPost)](https://www.marktechpost.com/2026/01/20/zhipu-ai-releases-glm-4-7-flash-a-30b-a3b-moe-model-for-efficient-local-coding-and-agents/)
- [GLM-4.7-Flash architecture notes (llm-stats)](https://llm-stats.com/posts/d9649b05-087d-4cbf-a45a-166ce2451e78)
- [Unsloth: GLM-4.7-Flash how to run locally](https://unsloth.ai/docs/models/tutorials/glm-4.7-flash.md)
- [llama.cpp issue 19068: GLM-4.7-Flash grammar trigger corruption](https://github.com/ggml-org/llama.cpp/issues/19068)
- [llama.cpp GBNF grammars](https://github.com/ggml-org/llama.cpp/blob/master/grammars/README.md)
- [SGLang cookbook: GLM-4.7-Flash](https://cookbook.sglang.io/autoregressive/GLM/GLM-4.7-Flash)
