# ago on gpt-oss via llama.cpp

Brainstorm: optimizations specific to running the ago language against
OpenAI's gpt-oss open-weight models served locally. Same framing as
deepseek.md and glm4.7-flash.md: levers only, no protocol changes,
ranked at the end. gpt-oss brings a different set of constraints than
either: a hard-required response format (harmony), native tool-calling
training, one fixed quantization, and a reasoning-effort dial the other
two models don't have.

## What these models are

Two sizes: gpt-oss-20b (21B total, 3.6B active, ~16GB, fits the R9700
with room) and gpt-oss-120b (117B total, 5.1B active, ~64GB at 8k
context, a 96GB+ Mac or 80GB GPU). Both ship natively in MXFP4; there
is no quantization ladder, you run the weights as trained. 131k
context, alternating dense and sliding-window attention with attention
sinks. Both are trained hard for agentic tool use, which makes them the
strongest native tool-callers in the local lineup, and both only work
through the harmony format: three channels, `analysis` (chain of
thought), `commentary` (tool calls and preambles), `final` (user-facing
text). Wrong or missing harmony structure does not degrade output, it
breaks the model.

## Harmony changes the cache and transcript math

- **Reasoning must be kept between tool calls and dropped after a final
  answer.** Harmony's multi-turn rule: previous turns' analysis content
  is removed, but within a turn (a chain of tool calls before the final
  message) the reasoning rides along. An ago episode is essentially one
  long turn: task in, dozens of tool calls, done. So the expensive
  drop-and-rewrite of history that would wreck prefix caching in a
  chatty workload almost never fires in ours. Verify the harness
  implements the rule correctly anyway; a client that strips analysis
  between tool calls degrades the model, and one that never strips it
  just spends tokens.
- **Tool calls arrive on the commentary channel with preambles.** The
  model narrates intent ("I'll rename the symbol first") before acting.
  That is generation-priced narration we didn't ask for; it is also
  what the model was trained to do, so suppressing it via prompt is
  fighting the training. Accept it, keep it short by keeping tool
  results terse and unambiguous (nothing to re-explain), and measure
  rather than assume it hurts.
- **Sliding-window attention interacts with llama.cpp's prompt cache.**
  SWA layers keep a windowed KV, which restricts how much of a prior
  prefix llama-server can reuse after divergence; `--swa-full` trades
  memory for full-cache behavior. For a retry-heavy protocol (rejection,
  repair, resubmit) that reuse matters; benchmark with `--swa-full` on
  the 20b, where memory headroom exists.

## Constrained decoding: worse than GLM, better than ds4

llama.cpp grammars apply at the raw token level, before harmony
post-processing. A grammar written against the clean API output fails,
because the model still has to emit channel markers; a grammar that
does account for them constrains the analysis channel too, which
lobotomizes reasoning. And grammar constraints collide with tool
calling, which already uses its own internal lazy grammars under
`--jinja`. Practical reading:

- Do not hand-write patch GBNF for gpt-oss. The structured expression
  form stays shelved here, same conclusion as ds4 but for format
  reasons rather than absence of the feature.
- The native tool-call path is the constrained decoder: gpt-oss was
  trained on function schemas rendered into its system prompt, and
  llama.cpp's harmony-aware template plus the internal tool grammar
  already force syntactically valid calls. Lean on that plus ago's
  validation, the same protocol-as-decoder stance as deepseek.md.
- `response_format` json_schema combined with reasoning was still being
  worked out upstream (llama.cpp issue 15276 lineage); check current
  server state before building anything on it.
- Same channel-leakage warning for the harness: clients that fail to
  filter harmony tokens corrupt tool arguments downstream. Verify
  opencode against a few tool calls by eye before trusting a bench run.

## Reasoning effort replaces the quant sweep

No quant ladder, but `reasoning_effort` (low, medium, high, set via
chat template kwargs) gives three capability-vs-token-budget points
from one set of weights, and 20b vs 120b gives the scale axis. The
interesting cells for the thesis:

- 20b at low effort is the weakest agent in the whole lineup that
  still tool-calls reliably. If semantic mode holds up there while raw
  mode collapses, that is the cleanest statement of the claim so far.
- Semantic mode may buy effort headroom: if 20b-low + ago matches
  20b-high + raw editing on pass rate, the protocol is worth two
  effort notches, which is a latency win as well as a capability one.
- 120b runs on the Mac, not the R9700, so cross-size comparisons are
  cross-hardware; report time-to-green per hardware, pass rate across.

## Sampling and serving

- OpenAI's recommendation is temperature 1.0, top_p 1.0; llama.cpp
  defaults add top-k 40 and min-p 0.1 on top. Pin one choice in the
  bench manifest; the silent divergence between "OpenAI recommended"
  and "llama.cpp default" is exactly the kind of thing that makes
  cross-model comparisons unfair.
- No repetition penalties, same reasoning as GLM: op JSON repeats keys
  and handles by design, and the serving guide says the same thing
  independently.
- Serve with `--jinja` and the ggml-org GGUFs, `-fa` on, `--ctx-size`
  bounded to the episode budget rather than 0 (max): SWA keeps KV
  growth modest (~0.2-0.3GB per 8k) but there is no reason to pay for
  131k when episodes should live under 32k.
- Disable the built-in browser and python tools in any harness that
  surfaces them; ago's tools are the whole surface, and gpt-oss will
  reach for python to edit files if offered an escape hatch. That is
  raw mode leaking into the semantic arm of the bench.

## Carry-overs that need no change

Byte-stable daemon responses, path hygiene in bench prompts, bounded
list responses with truncation markers, repairs and did_you_mean as
paste-ready op JSON: all apply verbatim (deepseek.md argues them; the
serving stack differences don't touch them). Flat sugar tools matter
here too, with a gpt-oss-specific reason: function schemas render into
the harmony system prompt in a TypeScript-like form, and deeply nested
`ops` payloads render into schema prose the model has to honor at
sampling time without a grammar backstop.

## Priority order

| lever | cost | expected payoff |
|---|---|---|
| verify harness harmony handling (CoT between tool calls, channel filtering) | audit | high: correctness of every run |
| disable built-in tools in the semantic arm | config | high: bench validity |
| pin sampling (resolve OpenAI-rec vs llama.cpp-default) | config | high for comparability |
| effort sweep (low/med/high) x mode | bench time | high: cheapest capability curve in the lineup |
| `--swa-full` for retry-heavy episodes | config + memory | medium |
| 20b vs 120b cross-size runs | Mac time | medium: scale axis |
| keep tool results terse to shrink commentary preambles | already policy | low-medium |
| patch GBNF grammar | n/a | skip: harmony makes it a trap |

## Open questions

- Does opencode implement harmony's keep-reasoning-between-tool-calls
  rule, and does it filter channel tokens from tool arguments? Both are
  correctness, not tuning.
- Has `response_format` json_schema alongside tool calling landed
  usable in current llama-server for harmony models?
- Do commentary preambles measurably slow episodes, and does prompting
  for brevity reduce them without hurting call accuracy?

## References

- [Introducing gpt-oss (OpenAI)](https://openai.com/index/introducing-gpt-oss/)
- [openai/gpt-oss on GitHub](https://github.com/openai/gpt-oss)
- [gpt-oss-120b on Hugging Face](https://huggingface.co/openai/gpt-oss-120b)
- [llama.cpp: running gpt-oss guide](https://github.com/ggml-org/llama.cpp/discussions/15396)
- [llama.cpp: gpt-oss and grammar](https://github.com/ggml-org/llama.cpp/discussions/15341)
- [Unsloth: gpt-oss how to run](https://unsloth.ai/docs/models/gpt-oss-how-to-run-and-fine-tune)
- [Harmony response format overview](https://cobusgreyling.substack.com/p/what-is-gpt-oss-harmony-response)
