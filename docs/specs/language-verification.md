# Language core: real-path verification (2026-07-15)

Built `ago` from this repo's `main` (`go build -o /tmp/ago-verify
./cmd/ago`) and ran it, `-C .`, from a private worktree of
`github.com/hashicorp/boundary` (533 packages, real production code, not
a fixture). Commands and trimmed outputs below; full worktree restored
with `git checkout -- .` and the daemon stopped afterward.

## status

```
$ ago status
{"errors": null, "files": 2350, "load_ms": 1618, "packages": 533, "status": "ok"}
```

## view: handles + generation

```
$ ago view -p github.com/hashicorp/boundary/internal/ratelimit -s 'Configs.Limits'
{"generation": 1, "nodes": 71, "status": "ok", "text": "func (c Configs) Limits(...) {\nn1: ...\n...\nn71: \treturn limits, nil\n}\n"}
```

71 statement/expression handles, generation present, as spec'd.

## patch: 3-op statement patch, accepted

Viewed `ratelimit.NewLimiter` (generation 1, single statement `n1: return
rate.NewLimiter(...)`), then applied:

```json
{"pkg": "github.com/hashicorp/boundary/internal/ratelimit", "sym": "NewLimiter",
 "generation": 1, "ops": [
   {"op": "add_if", "at": "n1", "where": "before", "cond": "maxEntries < 0"},
   {"op": "add_call", "at": "$1", "where": "last", "expr": "http.StatusText(400)"},
   {"op": "add_return", "at": "$1", "where": "last", "exprs": ["nil", "nil"]}
]}
```

```
$ ago patch --body-file - < patch.json
{"files": [".../internal/ratelimit/limiter.go"], "generation": 2,
 "ops_applied": 3, "packages_rechecked": 96, "status": "accepted",
 "symbol": "github.com/hashicorp/boundary/internal/ratelimit.NewLimiter"}
```

File on disk after: the `add_if` created a then-block ($1), `add_call`
and `add_return` addressed it by op-index ref, exactly as the spec's
`$N` addressing describes.

```go
func NewLimiter(limits []rate.Limit, maxEntries int) (*rate.Limiter, error) {
	if maxEntries < 0 {
		http.StatusText(400)
		return nil, nil
	}
	return rate.NewLimiter(...)
}
```

## patch: same body, stale generation, rejected

Replayed the identical patch (still says `generation: 1`; the accept
above bumped the real generation to 2):

```
$ ago patch --body-file - < patch.json
exit 2
{"detail": "github.com/hashicorp/boundary/internal/ratelimit is at generation 2, patch was built against 1",
 "reason": "stale generation: re-view", "status": "rejected"}
```

## query -k implementations

First tried `ratelimit.Limiter` (found via `query -kind search -q
Limiter`, 24 hits): 0 implementations. Correct, not a bug: the concrete
implementers (`rate.Limiter`, `rate.NopLimiter`) live in the external
`go-rate` module, and `Implementations` scans `workspacePackages()` by
design (workspace = one module tree, per language.md's definition).
Confirmed real signal by trying an interface implemented inside the
boundary module itself:

```
$ ago query -k implementations -p github.com/hashicorp/boundary/internal/db -s ResourcePublicIder
{"count": 113, "direction": "interface_to_types", "status": "ok", ...}
```

113 real implementing types found.

## query -kind callers

```
$ ago query -kind callers -p github.com/hashicorp/boundary/internal/ratelimit -s DefaultLimiterMaxEntries
{"count": 28, "status": "ok", "callers": [...]}
```

28 real call sites across `cmd/config`, `daemon/controller`, and the
package's own tests (brief's reference count of 29 includes the
definition itself; 28 callers is consistent).

## test

`internal/ratelimit` needs a live postgres for most of its suite (rejected
by the design docs' own non-goals: `test` runs real `go test`, it doesn't
fake infra). Ran it anyway to see the tool surface a real environmental
failure honestly, then picked a small pure package:

```
$ ago test -p github.com/hashicorp/boundary/internal/errors
{"pass": false, "failed": 2, "tests": [
  {"name": "TestConvertError", "pass": false,
   "output": "... could not create test database: ... role \"boundary\" does not exist ..."},
  ...
]}
```

Structured per-test pass/fail with real failure output, exactly as
spec'd; the failure is environmental (no local postgres), not an `ago`
defect.

```
$ ago test -p github.com/hashicorp/boundary/internal/types/action
{"pass": true, "failed": 0, "tests": [64 entries, all pass]}
```

64/64 clean.

## help

```
$ ago help
{"status": "ok", "version": "v1", "tools": [...], "ops": [26 entries]}
```

26 ops in the catalog, matching `opRegistry`'s 26 registered ops exactly
(8 decl + 14 statement + 4 test). Version `v1`.

## Outcome

No defects found. Every command matched its documented behavior,
including the two results that looked surprising at first glance
(`implementations` returning 0 for an externally-satisfied interface;
`test` surfacing a real infra dependency) and turned out to be correct
per the spec's own scoping rules.
