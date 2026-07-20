# Protocol surface: shipped and foreseen

The running list of every call an agent can make (or will be able to),
one row per tool and op. `TestSurfaceDocCurrent` asserts every op in the
live help catalog appears here, so shipped rows cannot drift. Statuses:
**shipped** (in the catalog today), **planned** (has a tracked issue),
**candidate** (named, waiting on evidence).

## Tools (the ten-call MCP surface, all shipped)

| tool | role |
|---|---|
| `status` | workspace snapshot: packages, files, errors |
| `help` | versioned op catalog with examples |
| `query` | search, inspect, refs, callers, callees, implementations, doc |
| `view` | declaration source with statement handles + generation |
| `patch` | atomic, generation-checked multi-op transaction |
| `test` | scoped go test with structured results |
| `rename` | one-op sugar over patch |
| `set_body` | one-op sugar over patch |
| `add_param` | one-op sugar over patch |
| `upsert_decl` | one-op sugar over patch |

## Patch ops

| op | status | notes |
|---|---|---|
| `rename` | shipped | capture-proof, atomic multi-rename; carries the doc comment's leading identifier |
| `set_body` | shipped | |
| `add_param` | shipped | inserts before a variadic tail (spread sites included); ceiling: func-as-value |
| `upsert_decl` | shipped | manages imports; creates packages on demand, including the first package of an empty module (module identity from go.mod); a first decl of `func main` names the package main; bare package names reject with the module-prefixed completion |
| `delete_decl` | shipped | |
| `set_doc` | shipped | |
| `add_field` | shipped | |
| `remove_field` | shipped | |
| `add_assign` | shipped | |
| `add_call` | shipped | expression statements only |
| `add_return` | shipped | |
| `add_defer` | shipped | |
| `add_go` | shipped | |
| `delete_node` | shipped | blocks must be empty |
| `add_if` | shipped | |
| `add_for` | shipped | no init/post clauses (v1 ceiling) |
| `add_switch` | shipped | |
| `add_case` | shipped | |
| `set_cond` | shipped | |
| `replace_expr` | shipped | condition / whole expression statement only |
| `wrap_stmts` | shipped | with: if, for, block |
| `wrap_error` | shipped | same-package arity resolution (v1 ceiling) |
| `add_test` | shipped | table-driven scaffold |
| `add_test_case` | shipped | |
| `set_test_case` | shipped | |
| `remove_test_case` | shipped | |
| `set_signature` | shipped | patch-only op; name-matched param carry, defaults for new params, spread-site insertion; interface methods pending |
| `move_decl` | shipped | self-contained decls, reference requalification; ceilings: package-local deps, types with methods |
| `remove_param` | planned | [agent-go-qhg] |
| `implement_interface` | planned | [agent-go-qhg] |
| `add_bench` | planned | [agent-go-qhg] |
| `add_dependency` | shipped | go get with byte-for-byte go.mod/go.sum restore |
| `remove_dependency` | shipped | go get @none, rejects while imported |
| `move_file` | shipped | same dir renames; cross package rewrites the clause, rejects while externally referenced |
| `delete_file` | shipped | rejects while the file declares referenced symbols |
| `mod_tidy` | shipped | restore-and-validate wrapper |
| `wrap_stmts with:go` | candidate | [agent-go-96n]; needs mined concurrency tasks |
| `guard_with_mutex` | candidate | [agent-go-96n] |
| channel send statement | candidate | [agent-go-96n]; `ch <- v` has no home today |

## Query kinds

List-returning kinds (search, refs, callers, callees, implementations)
are position-sorted and paged 50 entries at a time: `count` is always the
total found, and a truncated response carries `truncated: true` plus
`next_offset` to pass back as `offset`.

| kind | status | notes |
|---|---|---|
| `search` | shipped | |
| `inspect` | shipped | |
| `refs` | shipped | |
| `callers` | shipped | static call edges |
| `callees` | shipped | static call edges |
| `implementations` | shipped | both directions; no method-set candidate bridges (v1 ceiling) |
| `doc` | shipped | |
| SSA tier | planned | [agent-go-tpe]; concrete callers through interfaces (RTA/VTA), dead query kind |
| async call edges | candidate | [agent-go-96n]; distinguish `go f()` in callers/callees |

## Rejection channel (shipped)

`did_you_mean` bare candidates; `possible_repairs` complete paste-ready
calls for addressing misses, stale generations, unknown handles/ops, bad
keywords, missing required args (help fallback), undefined identifiers
(search fallback), filename-as-sym (search fallback); resend breaker
escalates exact resends daemon-wide.

Foreseen: structured expression nodes for constrained decoding
([agent-go-dzk]), handle migration across generations ([agent-go-4j5]),
subsequence matching for renamed symbols ([agent-go-oa9]).
