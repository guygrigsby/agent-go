# Model optimization research

Per-model brainstorms for running the ago language against local models.
Levers only, no protocol changes; each doc ranks its own priorities.
[cross-model.md](cross-model.md) synthesizes the levers that pay on
every family and is the build list.

## Scope: what local means

Local means what an average developer can run. The cap is a 128GB
unified-memory Mac (M3/M4 Ultra class); anything needing more is
extremely expensive and out of scope. Single-GPU boxes (R9700, 24-32GB
class) are the lower tier and serve the small end of each family.

## Covered

| doc | family | local variants in scope |
|---|---|---|
| [glm4.7-flash.md](glm4.7-flash.md) | GLM | 4.7-Flash (30B-A3B, ~18GB Q4); the GLM-5.x line is 753B and out |
| [qwen.md](qwen.md) | Qwen | Coder-Next (80B-A3B, ~52GB Q4), Qwen3.6-27B MTP (~17GB Q4), Qwen3.6-8B (\~5GB Q4, floor cell; fall back Qwen3.5-8B) |
| [deepseek.md](deepseek.md) | DeepSeek | V4 Flash (284B MoE) via ds4 selective 2-bit, ~81GB; V4 Pro out |
| [gpt-oss.md](gpt-oss.md) | gpt-oss | 20b (~16GB) and 120b (~64GB), native MXFP4 only |
| [devstral.md](devstral.md) | Devstral | Small 2 (24B, ~14GB Q4), 123B (~70GB Q4, Mac only) |

This set is the viable-model matrix for local dev as of 2026-07. The
bench roster built from it (tiers, roles, the 8B floor cell, fill
order) is in [cross-model.md](cross-model.md); the 8B exists to find
the protocol's capability floor, not to compete, and completes the
within-family Qwen scale curve (8B, 27B, 80B-A3B).

## Excluded at the cap, with reasons

- Kimi K2.x / K2.7 Code: 1T total parameters; nowhere near 128GB at any
  quant.
- MiniMax M3: 428B/26B active; smallest usable GGUF is 128GB before KV
  cache, so it loads and does not run. Revisit if the requested
  128GB-friendly Flash variant ships
  ([MiniMax-M3 issue 15](https://github.com/MiniMax-AI/MiniMax-M3/issues/15)).
- GLM-5.2: 753B. The GLM doc's scope stays the Air/Flash tier.
- Llama 4: generalist with historically weak tool calling; nobody
  picks it for agentic coding.
- Gemma 4: originally excluded here on Gemma 3 evidence (6.6% on
  tau2-bench retail); reversed in the cross-model roster. Gemma 4
  posts 86.4% with native function-calling tokens and joins as the
  generalist arm: tool mechanics without coding-agent post-training.
- Seed-OSS-36B, Nemotron 3 Nano, Granite 4: fit the cap, tool-call fine,
  but second tier for agentic coding and small install base among people
  who would run this bench. Skipped unless one starts showing up in
  local-coding usage.

Borderline, undecided: MiMo-V2-Flash (Xiaomi, 309B/15B active, MTP,
claims #1 open-source SWE-bench Verified). Same size class as DeepSeek
V4 Flash, so viable only through the same aggressive selective-quant
route; worth a doc if community quants prove out at ~80GB.

Open before any Devstral 123B bench run: measure real decode t/s at Q4
on the 128GB Mac first; that number decides whether the adversarial
cell (devstral.md) gets k=3 or k=1.

## Cross-cutting note: the "weak at choreography" premise moved

The bench thesis was framed against models that emit good code but
choreograph tools badly. The 2026 crop is different: Qwen3-Coder-Next,
gpt-oss, and Devstral 2 are all post-trained on agentic tool
trajectories, and DeepSeek V4 and GLM ship interleaved
thinking-before-tool-calls. "Local model" no longer implies "weak tool
caller"; the weak-choreography population is now the small dense tier
(Qwen3.6-27B and below, gpt-oss-20b at low effort). Bench arms that
test op granularity against model strength (semantic-coarse vs
semantic-full, effort sweeps) should be read with that split in mind,
and each doc calls out where its model sits on it.
