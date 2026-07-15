Tasks
---
- docs (Language for local models)
    - humans
    - agents
- agent language spec (whatever that looks like)
- README with quickstart and contributing
- roadmap
- task tracking solution
- make it pretty
- cross-model wins (optimizations/cross-model.md, ranked there)
    - engine
        - rejections carry literal next calls (paste-ready op JSON in possible_repairs / did_you_mean)
        - accept responses carry fresh {generation, view}
        - determinism invariant + byte-equality test
        - bounded list responses (caps, total count, truncation marker)
        - lenient patch decode, repair noted in response
        - resubmission circuit breaker (hash rejected patches, escalate)
        - unknown-tool/op aliases + no-shell redirect
        - help examples validated against fixture
        - no markdown fences in protocol output
    - bench/harness
        - per-model serving profiles, pinned + recorded in episode metadata
        - episode counters in JSONL (parse fails, decode fails, op mix, resubmits, tokens, time-to-first-mutation)
        - canary probe + restart-on-mismatch
        - prompt prefix hygiene (no paths/ids/dates, stable tool order)
        - harness resend audit per stack
    - AGENTS.md common skeleton with per-family variants


