# 5. First-party authoring driver

Status: Accepted (2026-07-19)

## Context

The bench proved the protocol against three local models, but every
episode so far rode opencode: an external host whose loop, prompts, and
built-in tools we neither control nor fully exclude. The frozen qwen
round recorded five invalid on-disk states in semantic episodes that
should be impossible by construction; the leading suspect is the host's
own edit tool leaking past the prompt. Prompt-level steering is not an
invariant.

Real authoring with local models needs two things the bench never did:
a driver whose tool surface is closed (nothing outside the protocol can
touch Go source), and a way to write the files the protocol does not
cover (go.mod hand-edits, README, configs, Makefiles). The op catalog
plus the query surface is already broad enough to start; the gaps that
matter will surface through use, not speculation.

## Decision

- Build a first-party driver in `internal/agent/`, exposed as
  `ago agent "<task>"`. One-shot first: task in, tool loop to
  completion under step and wall caps, JSONL transcript, diff summary
  out. Interactive mode later reuses the same loop with a stdin turn
  source.
- The driver serves the shipped tool surface in-process, the same code
  path as `ago mcp`, no socket roundtrip.
- Two driver-side tools extend the surface: `read_file` (any path) and
  `write_file`, which rejects `.go` paths mechanically. Go source is
  only reachable through validated ops; the invariant lives in code,
  not prompt. No shell tool in v1 (named ceiling); `test` covers go
  test and patch carries the build verdict.
- Serving stays external behind a one-method client interface
  (OpenAI-compatible chat completions with native tool calling), so
  llama.cpp, the llama-swap router, and hosted endpoints interchange.
  Profiles (endpoint, model, sampler, key env) live in
  `.ago/agent.json` with flag overrides.
- Missing ops discovered while dogfooding become tracked issues; the
  surface grows from recorded need, the same discipline the bench
  used.
- Development happens on the `authoring` branch in its own worktree;
  the eval-freeze-1 tree and running rounds are untouched.

## Consequences

- The zero-invalid-intermediates claim becomes enforceable: with a
  closed tool surface, a broken on-disk state in a semantic session is
  an engine bug, not a host leak.
- A first-party loop means owning prompt engineering, tool-call
  parsing quirks, and retry behavior per model family; opencode was
  absorbing that cost.
- Non-Go files get raw writes with no validation; that is the accepted
  boundary of the protocol, not a gap to close.
- The bench keeps riding opencode for comparability with recorded
  runs; the driver is an authoring tool, not a fourth bench arm, until
  a deliberate decision says otherwise.
