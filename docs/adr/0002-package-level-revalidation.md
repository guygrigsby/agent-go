# 2. Package-level revalidation with in-place splice

Status: accepted. Amends the revalidation consequence of 0001.

## Context

0001 accepted a full workspace reload after each accepted mutation (deferred
to the next query), 1.4-2s on boundary-scale repos. Guy rejected that:
builds after a change should be package-level, as whole-workspace work is
almost never necessary. The blocker was identity: queries matched objects by
defining position, and re-typechecking one package shifts positions, so
partial refresh would desynchronize cross-package references.

## Decision

Identity moves to `objectpath` (pkgpath + object path), which is stable
across re-typechecks, test-variant packages, and stale importers; locals
fall back to position, which is safe because locals are only referenced from
their own freshly-checked package.

On that footing, mutations re-typecheck only their dirty set, in dependency
order, splicing results into the snapshot in place (importers share package
pointers, so spliced types propagate automatically); any diagnostic rolls
back every splice and every file. Dirty sets: body edit → packages compiling
the edited file; rename → packages with rewritten files plus transitive
reverse importers of the target's package (a method rename can break
interface satisfaction in a package that never names the method). Full
reload remains only for cold start, external edits (mtime check), and cgo
packages in the dirty set.

## Consequences

- Boundary-scale measurements: rename touching 7 files / 29 refs rechecks
  75 packages in ~230ms end to end; the next query is ~ms with no reload.
  No operation in the agent loop pays whole-workspace cost anymore.
- The shared FileSet grows with every splice; daemon idle-exit bounds it.
- Rename's post-splice resolution check (capture/collision detection) can
  fail after a successful splice; recovery re-typechecks the same dirty set
  against the restored files rather than reloading.
