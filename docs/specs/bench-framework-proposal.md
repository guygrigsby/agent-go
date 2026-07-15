# Bench framework proposal

Proposal only; changes nothing. Reviewed against bench.md, plan.md,
language.md, `bench/bench_test.go` as of `bd18d50`, and the model
research in docs/optimizations/. The current framework is right at its
core: go test runner, one episode per iteration, opencode driving both
modes with real purity controls (tool denylists, XDG isolation, the
round-3 skill-leak fix), full evidence per episode, benchstat for
comparison. Keep all of that. The gaps are what the model-matrix work
(optimizations/cross-model.md) exposed: the bench can say pass or fail,
but not yet why, at what cost, under which serving config, or whether
the failure was the model's at all.

## 1. Failure taxonomy

`agent_error` is a bool and the first recorded episodes already show
why that is not enough: rows with `agent_error: true, capped: true`
cannot distinguish a model that ran out of time from an endpoint that
fell over at minute one. Replace with `failure_kind`:
`capped | endpoint_error | harness_crash | tool_parse | scored_fail`,
derived from exit codes, transcript tail, and the ctx deadline.
A run where endpoint errors cluster on one model is a serving problem,
not a result, and today it would be reported as a result.

Two supporting pieces from cross-model.md, both bench-validity rather
than features:

- **Canary probe.** A fixed prompt against the endpoint before each
  episode, output compared to a recorded known-good; on mismatch, run a
  configured restart command (`AGO_BENCH_RESTART_CMD`) and retry once.
  Motivated by GLM's wedge-while-health-check-passes bug; one silent
  corruption otherwise poisons a whole run in either mode.
- **Server identity in run.json.** llama.cpp or ds4 build, model file
  hash, and the full sampler config. The GLM scoring_func bug means a
  server version is part of a result's identity, same as the model.

## 2. Serving profiles replace the bare model env var

`AGO_BENCH_MODEL` + endpoint is not enough state to reproduce a run.
A `bench/profiles.json` with one named profile per model: endpoint,
model id, sampler (temperature, top_p, min_p, top_k, repeat penalty
explicitly 1.0), thinking/effort setting, quant label, context size,
notes (parser, template revision). Episodes carry the profile name;
run.json embeds the whole profile. The per-model docs each found a
silent misconfiguration that would read as model failure; profiles are
where those findings become pinned config instead of tribal knowledge.

Matrix runs follow: `AGO_BENCH_PROFILES=glm-flash,gpt-oss-20b` loops
profiles, and profiles on different endpoints (Mac vs R9700) can run
concurrently since episodes only contend on their own server. Today
that is two hand-launched invocations; it should be one.

## 3. Protocol-side evidence: the daemon logs the episode too

The biggest gap. Evidence today is opencode's view (transcript, diff,
score). The semantic arm has a second, better witness: the daemon sees
every query, view, patch, rejection, and repair. Add a per-workspace
request log (JSONL: op, args digest, outcome, rejection reason, repairs
offered, generation, latency) and copy it into the episode dir at
scoring time.

That log is what makes the cross-model counters computable without
parsing opencode's transcript format: op mix with per-op accept/reject,
identical-resubmission count, repair uptake (did the model send the op
a rejection offered), stale-generation events, time to first accepted
mutation. Those counters are the difference between "semantic mode
lost on GLM" and "GLM never recovered from unknown-handle rejections",
and the second sentence is the one that drives protocol changes.

Token accounting rides along, and the source question is settled:
opencode records per-assistant-message `tokens` (input, output,
reasoning, cache read/write) and cost in its sqlite db (verified
locally against a GLM-4.7-Flash message). The bench redirects
XDG_CONFIG_HOME but not data, so the db is queryable by session id
after each episode; the `--format json` event stream likely carries the
same message objects, confirm on the next smoke run. No server-side
scraping needed. Tokens in/out per episode belong in episodes.jsonl;
time under a cap is only half the cost story on local hardware.

Placement, per Guy's call: the daemon request log is a flag, off by
default; always-on is too expensive. Not a bench-only flag though, a
general daemon option (`log requests to <path>`), since an op-level
audit trail has uses beyond the bench (debugging agent sessions,
post-hoc "what did the agent actually do" review). The bench sets it
per workspace and collects the file at scoring time.

## 4. Scoring: pluggable predicates and an oracle arm

- `score()` is rename-shaped. Make the goal predicate a per-kind
  dispatch on `Manifest.Kind` (rename done; add-param, signature, move,
  wrap-error next, matching the planned prep extractors), so new task
  kinds are a predicate function plus an extractor, not a fork of the
  scorer.
- **Oracle arm.** The oracle harness already executes
  ground-truth-derived patches for every task (language.md, testing).
  Wire it in as a recorded pseudo-mode: per task, replay the oracle
  patches through ago, score identically. It proves each task is
  solvable through the protocol at all (a task the oracle fails is a
  bench bug), gives the floor for time-to-green that the cap should be
  calibrated against, and its transcripts are the SFT corpus the
  training path wants. One arm, three uses.
- **pass@k with intervals.** k=3 pass rates need binomial confidence
  intervals or every 2/3-vs-3/3 difference will get over-read.
  benchstat keeps wall-clock; a small report tool (below) owns rates.

## 5. Reporting as a subcommand, not a notebook

episodes.jsonl is accumulating; analysis today is ad hoc. A `report`
command under the bench package (Go, per the repo rule that a committed
script is a missing subcommand): reads one or more run dirs, emits the
comparison table (task x mode x model: pass@k with CI, median
time-to-green, capped counts, failure_kind breakdown, counter
summaries) as markdown, plus the completion-time curve data as CSV.
Results are committed; the report renders from them deterministically.

## 6. Prompt parity, measured instead of argued

plan.md flags prompt parity as open. Current configs give semantic a
workflow paragraph and raw one sentence. Two cheap moves: give raw an
equivalently sized workflow paragraph (its idioms: grep, edit, build),
and record both prompts' token counts in run.json so the delta is a
reported number. Parity by construction where possible, by measurement
where not.

## 7. Contamination: name it, don't fight it

Tasks are mined from public commits of popular repos; every model in
the matrix has likely trained on the ground-truth diffs. This does not
threaten the core claim, because the comparison is mode-vs-mode on the
same model and contamination is constant across arms. It does inflate
absolute pass rates. State it in bench.md's caveats, and if absolute
numbers ever matter (publication), adopt SWE-rebench's answer: mine
commits newer than the model's cutoff, which our miner already supports
by construction (it takes a repo and a date range).

## Harness: both, as a first-class axis

Guy's call: opencode and Pi are both very popular, both get tested.
The harness becomes a recorded dimension of every episode
(`harness: opencode|pi`), not a footnote, and the report compares
across it. If the mode margin holds under both drivers it is a protocol
result, not a harness artifact; if it doesn't, that divergence is
itself a finding about harness sensitivity, and either way the bench
speaks to both user populations.

Both are installed and both record per-message token usage in
inspectable local files (opencode: sqlite db; Pi: session JSONL with
`usage` per entry; both verified locally). What each needs:

- **opencode**: already wired. MCP-native, purity solved (tool
  denylist, XDG isolation, the round-3 skill-leak fix), continuity with
  recorded results.
- **Pi** (0.80.2, already serving Qwen locally here): extension-based
  by design, and MCP-server extensions exist, so ago's tools arrive
  either through an MCP extension or a thin native-tool extension
  calling the ago CLI; test the MCP route first since it reuses the
  exact server opencode runs. Purity is first-class on the CLI
  (`--no-tools`, `--tools` allowlist, `-ne -ns -nc -na`), which
  replaces opencode's config dance with flags, and `--thinking
  off..xhigh` maps directly onto the thinking bench axis. `--mode
  json`/`--mode rpc` cover programmatic driving.

Sequencing still matters under time-box: bring Pi up on the rename
round with one model first (validate the ago extension route, purity,
and evidence capture), then widen to the matrix. Full model-x-harness
is the target grid; corners get filled in the roster's fill order.

## What not to build, after looking outside

- **Harbor / Terminal-Bench** (containerized agent evals, parallel
  execution, trajectory conversion): the right tool for cross-agent
  leaderboards on machine-per-task workloads. Our tasks are git
  worktrees on two known boxes, the runner is 400 lines of Go we fully
  control, and container isolation buys nothing until the bench outgrows
  local hardware. Steal its idea of converting native agent logs into a
  common trajectory shape (that is section 3), not the framework.
- **Inspect AI** (UK AISI): mature scoring/epochs/log-viewer, but a
  Python framework wrapping a Go bench that already has a runner,
  and its value (solvers, sandboxes) duplicates what opencode and
  worktrees do here. Borrow the epoch/CI discipline (section 4).
- **SWE-rebench V2**: validates the mining approach (real commits,
  automated validation, decontamination by recency) at 100x our scale
  for Python/RL. Our compiler-oracle validation is stronger than their
  LLM-judge filter for our task kinds; nothing to adopt beyond the
  decontamination framing (section 7).
- **SWE-smith-style synthetic task generation**: interesting for
  training data volume later (the qwen doc's SFT path), not for the
  bench; synthetic tasks would undercut the mined-from-real-commits
  credibility that bench.md is built on.

## Priority

| change | size | why first |
|---|---|---|
| failure taxonomy + canary + server identity | small | every run's validity depends on it |
| daemon request log into episode evidence | medium | unlocks all six counters, explains failures |
| serving profiles + matrix loop | small | reproducibility, multi-model rounds are imminent |
| per-kind predicates | small | add-param and move rounds are next in plan.md |
| oracle arm | medium | task validity + cap calibration + SFT corpus |
| report subcommand with pass@k CIs | small | results exist, analysis shouldn't be ad hoc |
| prompt parity + contamination caveat | trivial | closes two open questions in plan.md |

## Open questions

- Is one canary probe per episode enough for GLM, or does the wedge
  strike mid-episode often enough to need a mid-episode heartbeat?
- Does opencode's `--format json` event stream include the message
  token fields, or does the bench query the sqlite db by session id?
  Either works (the db is confirmed); pick on the next smoke run.

Resolved (Guy, 2026-07-15): daemon request log is a general daemon
flag, off by default, not always-on and not bench-only. Token usage is
recorded by both harnesses locally; no server scraping. Harness: both
opencode and Pi run as a first-class bench axis; Pi's ago tools come
via its extension model (MCP extension route first).

## References

- [Terminal-Bench / Harbor](https://github.com/benchflow-ai/awesome-evals) (framework survey);
  Harbor evaluates containerized agents with parallel execution and log-to-trajectory conversion
- [SWE-rebench: automated task collection and decontaminated evaluation](https://arxiv.org/abs/2505.20411)
  and [SWE-rebench V2](https://www.emergentmind.com/papers/2602.23866)
- [Inspect AI evals (UK AISI)](https://github.com/UKGovernmentBEIS/inspect_evals)
- [EvoCode-Bench: multi-turn iterative coding agent evaluation](https://arxiv.org/pdf/2605.24110)
