# Cross-model wins

Synthesis of the five model docs (deepseek, glm4.7-flash, gpt-oss, qwen,
devstral): the levers that appear in every doc, or fall out of economics
shared by every stack, and are therefore worth implementing once rather
than per model. Per-model tuning stays in the per-model docs; this is
the build list.

Shared economics first, since every universal lever traces back to them.
On all five stacks generation runs 15-20x slower than prefill, cached
prefix is free or nearly so, and validation is server-side and cheap
relative to a model turn. So anything that (a) shrinks what the model
must emit, (b) keeps the transcript byte-stable, or (c) removes a round
trip, pays on every model regardless of family quirks. The levers that
depend on a family quirk (grammars, MTP, effort dials, harmony rules)
are per-model and stay out of this list.

## Engine work

In rough priority order. All are stack-independent: they live in the
daemon and protocol, and no serving-stack change can substitute for
them.

1. **Rejections carry literal next calls.** `possible_repairs` and
   `did_you_mean` as complete, paste-ready op JSON, never prose advice
   or bare names. Every doc lands on this independently: copying beats
   composing for weak emitters, copied spans are what MTP drafts and
   greedy decoding reproduce, and it is the constrained decoder for the
   three stacks where real grammars are unavailable or unsafe.
   Partially done (repairs work started); finishing it to the
   "executable verbatim" bar is the single highest-value item here.
2. **Accept responses carry the fresh view.** Mutations return
   `{generation, view}` for the touched declaration so the
   edit-then-edit-again loop needs no `view` call between patches.
   Every round trip removed is a prefill saved on every stack; this is
   the cheapest structural token win available.
3. **Determinism as a tested invariant.** Identical query against an
   unchanged snapshot returns identical bytes: sorted keys, stable
   ordering, no timestamps, no volatile counters. Prefix caching on
   every stack (ds4's SHA1 byte prefix, llama-server and vLLM token
   prefix) hangs on this, and nothing enforces it today. One test:
   run every query twice against the fixture, assert byte equality.
4. **Bounded list responses.** refs/callers/search capped with a total
   count and an explicit truncation marker, pageable. Protects the
   prefill budget everywhere and keeps a popular symbol from dumping
   400 sites into a weak model's context.
5. **Lenient patch decode, repair noted.** Accept literal newlines and
   tabs inside JSON strings, trailing commas, single-quoted keys;
   report the normalization in the response. Code-bearing string args
   (`set_body`, `upsert_decl`) are the most likely "semantic mode
   loses" mechanism on every small model, and this absorbs the most
   common damage class without a grammar.
6. **Resubmission circuit breaker.** Hash rejected patches; an exact
   resend of a just-rejected patch gets an escalated rejection (shorter
   reason, harder imperative naming the recovery call). The
   loop-until-cap failure mode is family-independent.
7. **Unknown-tool and unknown-op redirects.** Alias the obvious name
   variants (`ago_view`, `get_view`), and answer hallucinated shell-ish
   tools (`bash`, `str_replace`, `execute_command`) with a one-line
   redirect: no shell here, mutations go through patch ops. Every
   family hallucinates differently; the agentic-trained ones
   (Devstral, gpt-oss) reach for shells specifically, so the redirect
   targets the training prior, not just the typo.
8. **Help examples validated against the fixture.** Every catalog entry
   carries one minimal accepted-against-the-fixture patch, because
   few-shot imitation is how every model in the lineup uses `help`.
   Task 12 asserts examples parse; extend to "would validate."
9. **No markdown fences in protocol output.** Models mirror formatting;
   fenced Go in a view invites fenced Go inside a JSON string. Plain
   text everywhere in views, help, and rejections.

## Bench and harness work

Universal because they protect result validity for every model, and the
per-model docs each discovered a reason to want them.

1. **Per-model serving profiles.** Parser, template revision, sampler,
   quant, thinking/effort setting, grammar on/off: one named profile
   per model, pinned in the manifest, recorded in episode metadata.
   Every doc found at least one silent misconfiguration that would read
   as model failure (wrong tool parser, wrong scoring_func, inherited
   repetition penalty, template drift). Repetition penalty off is a
   profile default for the lineup: op JSON repeats keys and handles by
   design, and every family's guide says disable it independently.
2. **Episode counters in the JSONL.** Tool-call parse failures, JSON
   decode failures (and whether lenient repair fixed them), op mix
   with per-op accept/reject, identical-resubmission count, tokens in
   and out per turn, time to first accepted mutation. Every
   optimization in every doc is a hypothesis; these counters are the
   only way to see which ones matter per family without re-running
   everything.
3. **Canary probe with restart-on-mismatch.** A fixed probe completion
   before each episode, checked against known-good output, server
   restarted on failure. Motivated by GLM's wedge-while-healthy bug,
   but health-check-lies is a serving-stack risk everywhere, and one
   silent corruption poisons a whole run in either mode.
4. **Prompt prefix hygiene.** No worktree paths, task ids, or dates in
   the system prompt or tool catalog; stable tool ordering in
   `tools/list`; byte-stable `help` per catalog version. Makes k=3
   episodes share a warm cache on every stack.
5. **Harness resend audit.** One work item, per-stack checklists:
   tool results resent byte-identical, results on the tool role (never
   a second user message), reasoning-block handling matching each
   model's rule. Cheap to verify, and a failure invalidates whichever
   arm it lands in.

## AGENTS.md as a per-family template

`ago init` writes protocol instructions; the shape that works is the
same everywhere (short, imperative, example-led, one worked
query-view-patch-reject-repair loop, an explicit "no shell, no file
editor" line) with a per-family register tweak on top. Implement the
common skeleton once with named variants, not five hand-maintained
files.

## Deliberate non-builds

- **Full patch GBNF grammar, as a dependency.** Only two stacks can use
  one safely today (llama.cpp for Devstral, gated for GLM behind the
  canary; vLLM xgrammar for Qwen), ds4 has no grammar support, and
  harmony makes it a trap on gpt-oss. Grammar is per-stack
  acceleration, never a correctness dependency: the protocol must work
  grammar-free, which the rejection-as-repair channel already provides.
- **Structured expression form, for now.** Designed in, deliberately
  unimplemented: its consumers (constrained decoding integrations) are
  the per-stack items above. Nothing universal is waiting on it.

## The long game, still universal

The oracle harness is a model-agnostic training-data generator: oracle
solutions rendered as episode transcripts give SFT data, rejected-then-
repaired pairs from bench episodes give preference data, and perturbed
oracle patches give rejection-recovery data. The rendering differs per
chat format; the generator is one piece of work. A versioned, stable op
catalog is a fine-tuning target in a way raw editing can never be, and
that holds for every family at once.

One honest tension to carry into the bench rather than resolve on
paper: deepseek.md argues for more flat single-op tools (catalog is
prefix-cached, emission is not), qwen.md argues for a small tool count
with op detail behind `help` (instruction-following degrades with
catalog size). Both are right for their family. Keep the spec's
ten-tool surface as the default, and let the op-mix and parse-failure
counters say whether any family earns a wider flat surface.

## Which model fits the protocol best: a prediction

Worth writing down before results exist, so the bench can confirm or
embarrass it. "Responds best to the ago way of working" decomposes into
four measurable things: schema-faithful tool calls, imitation of
paste-ready examples (the repair loop is built on copying), working
inside a fixed vocabulary instead of fighting it, and a serving stack
where ago's levers land.

Predicted order:

1. **Qwen3-Coder-Next.** Hits all four. Agentic-trained on generic
   trajectories, not a specific shell scaffold, so no prior to unlearn.
   The family leans on few-shot imitation, which is exactly the grain
   of repairs-as-literal-calls and fixture-validated help examples: ago
   amplifies its strengths rather than compensating for weaknesses.
   Only family where the full constrained-decoding story works today
   (xgrammar, GBNF, no harmony trap, no wedge bug); effectively the
   model the structured expression form was designed for.
2. **gpt-oss-120b.** Strongest native tool-calling discipline in the
   lineup, plus the effort dial. Two deductions: trained with built-in
   browser/python tools, so it reaches for python to edit files when
   any escape hatch exists, and harmony rules out grammar assistance,
   so it runs on protocol validation alone.
3. **DeepSeek V4 Flash.** Best stack alignment (DSML temp-0 structure,
   ds4 exact replay) but locally it only exists at 2-bit through a
   beta engine: the best test of the thesis, not the best responder.
4. **GLM-4.7-Flash.** Fine incumbent; serving fragility taxes it.
5. **Devstral 2.** The anti-fit by construction: most skilled coder
   here, post-trained specifically on raw-mode choreography, most
   likely to fight the protocol. That is why devstral.md frames it as
   the adversarial control.

The distinction that matters when reading results: best responder is
not where the thesis wins biggest. The largest semantic-vs-raw margin
should show on the weak tier (gpt-oss-20b at low effort, Qwen3.6-27B,
GLM-Flash at Q2), because that is the population ago exists for.
Coder-Next is the model that will make ago look most fluent; the small
dense models are the ones that will make it look most necessary.
