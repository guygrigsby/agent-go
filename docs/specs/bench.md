# Bench: raw editing vs semantic protocol

Same local model, same hardware, same tasks, two modes. The claim under test:
a weak model completes repo-wide Go changes faster and more often when
restricted to semantic queries and validated mutations than when given raw
shell and file editing.

## Tasks

Mined from real commits (traefik, vault, boundary), never hand-written.
`bench/mine.py` finds them; `bench/candidates.json` holds 83 modules-era
candidates across kinds: rename, add-param, signature, move, wrap-error.

A task is usable when its parent commit typechecks clean with the current
toolchain. Task = repo, sha, kind, prompt, worktree checked out at `sha^`.
The commit itself is ground truth but is never diffed against; it exists to
derive the prompt and the goal predicate.

Prompt = the commit subject, lightly cleaned (strip issue refs, ticket ids).
Subjects of mined refactor commits are already what a user would type:
"Rename MaxEntries to MaxQuotas".

## Scoring

A run passes when all three hold within the time cap:

1. **Goal predicate** — the change happened. Derived mechanically from the
   ground-truth commit, checked with `ago` queries:
   - rename: old symbol gone; new symbol exists with the old reference count
   - add-param: target signature includes the new parameter
   - signature: target signature matches ground truth's
   - move: symbol's defining package changed to the target package
   - wrap-error: new error path present at target sites
2. **Typecheck clean** across the workspace.
3. **Tests pass**, scoped to packages the ground-truth commit touched.
   Full suites are out: vault and boundary want docker and tens of minutes.

Time cap: fixed per task, same in both modes (it is a comparison; scaling
the cap by task size adds nothing). Every attempt records time-to-green, so
results report the full completion-time curve; the cap is one point on it.

Nondeterminism: k runs per task per mode (k=3 to start), report pass rate
and median time-to-green.

## Modes

- **raw**: shell, file read/write, `go build`. No `ago`.
- **semantic**: `ago` only. No raw file writes; freeform code enters through
  body-payload ops, compiler-checked before touching disk.

Expected failure modes raw should exhibit and semantic should not: missed
callers, wrong-symbol edits, broken imports, long compile-repair loops.

## Open

- Task set: validate all 83 parents, select a balanced ~30 (boundary-heavy;
  traefik contributes little post-modules).
- Cap value: pick after watching a few runs; err generous, the curve does
  the discriminating.
