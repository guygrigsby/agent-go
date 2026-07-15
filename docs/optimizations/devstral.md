# ago on Devstral 2 via llama.cpp/vLLM

Brainstorm: optimizations specific to running the ago language against
Mistral's Devstral 2 models served locally. Same framing as the sibling
docs: levers only, no protocol changes, ranked at the end. Devstral's
distinct property in this lineup: it is the one family post-trained
specifically for agentic coding scaffolds, which makes it both the
strongest native tool-caller here and the most likely to fight the
protocol with its own training.

## What these models are

Two sizes, both dense, both 256k native context:

- Devstral Small 2 (24B, Apache 2.0): ~14GB at Q4_K_M, fits the R9700
  and any 24GB card. The realistic bench driver.
- Devstral 2 123B (modified MIT): ~70GB at Q4, Mac-only in our scope.
  72.2% SWE-bench Verified, top of the open-weights class at release.

Tool calling is native and dual-format: Mistral function calling or an
XML fallback, parsed by `--tool-call-parser mistral` in vLLM
(mistral_common 1.8.6+) or `--jinja` in llama.cpp. Parallel tool calls
are supported and the model uses them unprompted. Recommended
temperature is 0.15, the lowest in the lineup.

## The control-group problem: trained for raw mode

Devstral's post-training is SWE-agent-shaped: explore with shell-ish
tools, read files, apply string-replacement edits, run tests, iterate.
That is raw mode. Two consequences, one methodological and one
practical:

- **Methodological: Devstral is the adversarial test of the thesis.**
  Every other model in the lineup is weakest at exactly what ago
  replaces (choreography, whole-repo recall). Devstral was drilled on
  raw-mode choreography, so its raw arm should be the strongest raw arm
  we can field locally, and the semantic margin should be smallest
  here, plausibly negative on the 123B. Run it anyway and report it
  straight: "semantic mode helps weak models and does not hurt strong
  agentic ones" is a fine result, and "loses to trained raw-mode
  behavior on Devstral-123B" would be a scoping fact worth knowing.
  The 24B is the interesting cell: agentic training at a size where
  exact recall is still weak.
- **Practical: the semantic arm must close every escape hatch.** A
  model trained to reach for `bash` and file editors will try to
  express those habits through whatever tools exist. Expect creative
  misuse: `set_body` used as a file writer, `upsert_decl` fed whole
  files, attempts to call tools that don't exist (`str_replace`,
  `execute_command`) which the harness must surface as clean unknowns,
  not silent drops. The unknown-op rejection with `did_you_mean` earns
  its keep here; consider the MCP layer returning a one-line "this
  workspace has no shell; mutations go through patch ops" on any
  hallucinated shell-ish tool name, a redirect aimed at the training
  prior rather than a generic error.
- **AGENTS.md for Devstral speaks scaffold, not manual.** Its training
  format has a system-prompt section describing available tools and an
  expected loop. Mirror that register: short tool list, one explicit
  loop (query, view, patch, on rejection repair and resubmit), an
  explicit "there is no file editor and no shell" sentence up front.
  Redirecting a trained prior beats documenting around it.

## Dense economics: the MoE math inverts

Every other family in the lineup is MoE with 3-15B active. Devstral is
dense: 24B or 123B active every token. On the Mac the 123B generates
roughly an order of magnitude slower per token than DeepSeek V4 Flash's
active-11B-class decode; on the R9700 the 24B decodes slower than
GLM-4.7-Flash despite similar quality claims.

- Token-lean emission stops being a nicety and becomes the whole
  latency story: 150 tokens of op JSON vs 800 tokens of file body is a
  5x wall-clock difference per mutation on dense hardware. The
  semantic mode's terse-output advantage is largest on this family;
  say so in the bench writeup when reporting time-to-green.
- Speculative decoding is worth real effort here, unlike GLM (fast
  anyway) and unlike gpt-oss (already cheap). Dense target + no MTP
  heads means a classic draft model: Ministral 3B as drafter for the
  24B, the 24B as drafter for the 123B on the Mac. Op JSON's
  predictability is the best case for draft acceptance.
- Prefill is also dense-priced. The prefix-cache discipline from
  deepseek.md (byte-stable results, stable catalog, no volatile prompt
  content) carries over verbatim and matters more per miss.

## Sampling and serving

- Temperature 0.15 recommended by Mistral, and it suits the protocol:
  op emission is nearly deterministic out of the box, no grammar
  needed to get stable JSON shape on the happy path. Pin it; do not
  inherit a harness default of 0.7.
- Pick one tool-call format and verify it end to end. Dual-format
  support means dual failure modes: a harness that advertises OpenAI
  style gets the XML fallback, and a parser expecting Mistral function
  calling mangles it. vLLM: `--tool-call-parser mistral` with
  mistral_common pinned 1.8.6+. llama.cpp: current build, `--jinja`,
  and confirm the template matches the 2512 revision (Mistral templates
  drift between revisions and llama.cpp bundles lag).
- Parallel tool calls are native: Devstral batches its own queries
  when the tools are read-only and the instructions permit it. Let it:
  the AGENTS.md "batch your query calls" guidance from qwen.md applies
  with no prompting cost, this model already behaves that way.
- Constrained decoding is available with no gpt-oss-style format
  collision: llama.cpp GBNF and vLLM structured outputs both work on
  Mistral-format models. Same incremental adoption as the GLM doc:
  json_schema on sugar tools first, full patch grammar later. At temp
  0.15 the marginal win is smaller than on GLM; measure before
  investing.

## Fine-tune target

Devstral Small 2 is arguably the best adapter target in the entire
lineup: dense (cheap, well-understood LoRA), Apache 2.0 (no license
friction on publishing adapters), already agentic (the adapter teaches
the ago vocabulary, not tool use from scratch). The oracle-trace SFT
plan from qwen.md applies unchanged, with one addition specific to this
family: include negative examples that redirect shell-reflex behavior
(hallucinated `bash`/editor calls answered by rejection, followed by
the correct op), since that reflex is in the base weights and the
adapter's job is partly to overwrite it.

## Priority order

| lever | cost | expected payoff |
|---|---|---|
| verify tool-call format end to end (parser, template revision, mistral_common pin) | audit | high: dual-format is a silent mangler |
| close escape hatches + shell-redirect rejection message | small | high: bench validity for the semantic arm |
| pin temp 0.15, no repetition penalty | config | high, free |
| scaffold-register AGENTS.md variant | small | medium-high: redirects the training prior |
| draft-model speculative decoding (Ministral 3B / 24B) | moderate setup | medium-high on dense hardware |
| prefix-cache discipline (carry-over) | verification | medium, priced higher per miss here |
| adapter on Small 2 with shell-redirect negatives | training run | medium-long term |
| json_schema on sugar tools | small | medium: less urgent at temp 0.15 |
| 123B Mac runs | Mac time | medium: the adversarial thesis cell |

## Open questions

- Which tool-call format does opencode negotiate with Devstral 2, and
  does the chosen parser round-trip parallel calls correctly?
- How often does the 24B hallucinate scaffold tools (`bash`,
  `str_replace`) inside the semantic arm, and does the redirect
  rejection actually reduce repeat attempts? Countable from episode
  JSONL with the unknown-op counter.
- Does the modified MIT license on the 123B carry any term that
  matters for publishing bench results or fine-tuned adapters? The
  24B's Apache 2.0 sidesteps the question if so.
- Real decode t/s for 123B Q4 on the 128GB Mac: is the adversarial
  cell affordable at k=3, or does it get k=1?

## References

- [Devstral-Small-2-24B-Instruct-2512 on Hugging Face](https://huggingface.co/mistralai/Devstral-Small-2-24B-Instruct-2512)
- [Devstral-2-123B-Instruct-2512 on Hugging Face](https://huggingface.co/mistralai/Devstral-2-123B-Instruct-2512)
- [Unsloth: Devstral 2 how to run](https://unsloth.ai/docs/models/tutorials/devstral-2)
- [Cline: Devstral 2 release notes](https://cline.bot/blog/devstral-2-release)
- [Devstral 2 review (Local AI Master)](https://localaimaster.com/models/devstral)
