# 3. Bounded contexts: one package, named seams

Status: accepted (2026-07-16)

## Context

DDD audit of the full tree (48 non-test files, ~12k LOC). The domain
splits into five bounded contexts:

1. **Workspace** (core): the typechecked snapshot. Loading, parallel
   retypecheck, splice, dirty sets, generations, diagnostic baselines.
   Invariant: the snapshot reflects disk, and no accepted change
   introduces a new diagnostic.
2. **Edit language** (core): the op catalog and the patch transaction.
   The Patch is the aggregate root; every mutation, sugar included, is a
   patch. Invariants: atomicity, generation checks, resolution proofs.
3. **Repair conversation**: rejections as data, paste-ready repairs,
   escalation, the help catalog. Invariant: an offered repair executes
   clean.
4. **Transport** (supporting): daemon socket, CLI, MCP. Adapters only.
5. **Bench** (separate subdomain): tasks, oracle, episodes,
   certification, evidence.

Audit findings:

- Layering is clean: cmd/ago to daemon to snapshot, protocol as a shared
  kernel leaf, nothing reaches up.
- Bench already sits behind a natural anti-corruption layer: it drives
  the engine only through the shipped binary's JSON surface, never by
  importing the engine.
- Contexts 1 through 3 share `internal/snapshot` (20 files). Generations
  and rejection shaping appear in nearly every op file: they are the
  ubiquitous language of the edit context, not misplaced concerns.
- The exported surface was wider than the bound surface: seven query
  methods were exported but reached only through the `Query` facade.
- Vocabulary drift: op/verb/tool across layers, `Def` vs `default`,
  `Reject` returned as `error`.

## Decision

Keep contexts 1 through 3 in one package. Ops need the snapshot's
unexported internals (fset, package graph, findObject, retypecheck); a
package split would export the engine's guts or force an interface
layer, real churn on the most test-verified code in the repo for no new
invariant. The boundaries live as file families instead: `snapshot.go`
and `generation.go` (workspace), `patch.go` and `ops_*.go` (edit
language), `repairs.go` and `help.go` (conversation), `queries.go` and
`view.go` (reads).

Consequences applied with this ADR:

- The seven query methods are unexported; `Query` is the one read
  facade the transport binds.
- The CLI speaks "op" like the wire and the engine. MCP keeps "tool":
  that is MCP's own vocabulary, and `resolveToolName` is the explicit
  translation at the adapter boundary.
- `protocol.Request.Default` matches its wire name.
- `Reject` implementing `error` is deliberate and stays: Go callers get
  idiomatic errors, the domain keeps its language, `errors.As` bridges.

## Consequences

Revisit the package split when one of these lands: the SSA query tier
(a second read model), multi-module workspaces (a second workspace
shape), or `snapshot.go` outgrowing roughly 1,500 lines. Until then the
file families and this record are the boundary. The function-size
hotspots (`patchComposable`, `moveDeclEdits`, `retypecheck`, each ~200
lines) are accepted: linear, heavily commented pipelines; splitting them
adds indirection without protecting any invariant.
