# 1. Resident snapshot daemon behind an unchanged CLI

Status: accepted

## Context

The spike measured per-invocation `packages.Load` on real repos: 0.6s on
x/tools (534 files), 1.5-4s on traefik, 2-6s on boundary (976 files, 231
packages). `go build ./...` as a validation substitute is worse: its no-op
staleness walk alone costs 4.5-6.5s on boundary. Guy set the tolerance:
seconds per operation is too much for an agent loop. Zero (zerolang) avoids
a daemon because its compiler reloads a purpose-built graph store in
milliseconds; Go's toolchain has no equivalent cold path.

## Decision

`ago` stays a CLI speaking JSON; each workspace gets a resident daemon the
CLI auto-spawns on first use (unix socket keyed by workspace root, idle
exit). The daemon holds the typechecked snapshot. Queries walk memory.
Mutations validate by re-typechecking only the target package against the
in-memory dependency graph (body-level edits cannot change a package's
exported API, so reverse importers are unaffected); accepted edits write to
disk and mark the snapshot stale, and the next query pays one reload.
External edits are caught by per-request mtime checks. Precedent: gopls
`-remote=auto`, the Go build cache — resident state as invisible
implementation detail.

Not chosen: gopls as the engine (its refactorings live in internal
packages; LSP gives edits and error strings, not typed rejections with
repair suggestions), and hand-rolled incremental typechecking at file
granularity (that is gopls's decade of work; package-granularity restore
via reload is enough until bench data says otherwise).

## Consequences

- Queries and rejections are fast (ms to ~300ms); an accepted mutation
  defers a full reload (2-6s on boundary-scale) to the next query.
  Consecutive mutations batch into one reload. If bench time-to-green is
  dominated by these reloads, the upgrade path is package-graph splicing.
- One daemon per workspace; requests serialized. Crash recovery is
  respawn-and-reload; no persistent state beyond the socket.
- The bench's raw-vs-semantic comparison is unaffected: raw mode never had
  a daemon, semantic mode's daemon is invisible to the agent.
