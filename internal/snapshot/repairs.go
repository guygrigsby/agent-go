package snapshot

import (
	"encoding/json"
	"go/types"
	"strings"
)

const maxRepairs = 3

// viewCall builds the complete view invocation for an address.
func viewCall(pkgPath, sym string) map[string]any {
	return map[string]any{"tool": "view", "args": map[string]any{"pkg": pkgPath, "sym": sym}}
}

// viewRepairs fills rej.PossibleRepairs with complete view calls built by
// substituting the missed part of the address with each did_you_mean
// candidate. Only substitutions View itself would accept survive: a repair
// that rejects when pasted back is worse than none. Caller holds mu.
func (s *Snapshot) viewRepairs(rej *Reject, pkgPath, sym string) {
	for _, c := range rej.DidYouMean {
		pkg, symc := pkgPath, sym
		switch rej.Reason {
		case "package not found":
			pkg = c
		case "receiver type not found":
			_, name, _ := strings.Cut(sym, ".")
			symc = c + "." + name
		default:
			symc = c
		}
		if !s.viewable(pkg, symc) {
			continue
		}
		rej.PossibleRepairs = append(rej.PossibleRepairs,
			Repair{Why: pkg + "." + symc + " resolves", Call: viewCall(pkg, symc)})
		if len(rej.PossibleRepairs) == maxRepairs {
			return
		}
	}
}

// patchOpRepairs adds the recovery call for op-level rejects a fresh view
// fixes: a missed handle means the caller's handle table is stale or
// invented, and the literal next call is the view that rebuilds it.
// Caller holds mu.
func (s *Snapshot) patchOpRepairs(rej *Reject, env patchEnvelope) {
	if rej.Reason == "unknown handle" && env.Sym != "" && s.viewable(env.Pkg, env.Sym) {
		rej.PossibleRepairs = append(rej.PossibleRepairs,
			Repair{Why: "rebuilds the handle table for " + env.Pkg + "." + env.Sym,
				Call: viewCall(env.Pkg, env.Sym)})
	}
}

// patchCall rebuilds the complete patch invocation from a parsed envelope
// with op index i's name replaced by cand. Only envelope fields are echoed,
// so transport extras in the original bytes are dropped.
func patchCall(env patchEnvelope, i int, cand string) (map[string]any, bool) {
	ops := make([]any, len(env.Ops))
	for j, raw := range env.Ops {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		ops[j] = m
	}
	ops[i].(map[string]any)["op"] = cand
	args := map[string]any{"pkg": env.Pkg, "ops": ops}
	if env.Sym != "" {
		args["sym"] = env.Sym
	}
	if env.Generation != 0 {
		args["generation"] = env.Generation
	}
	if env.DryRun {
		args["dry_run"] = true
	}
	return map[string]any{"tool": "patch", "args": args}, true
}

// viewable mirrors View's acceptance: the address resolves and is not a
// field-of-type selector, which View redirects to the containing type.
func (s *Snapshot) viewable(pkgPath, sym string) bool {
	_, obj, rej := s.findObject(pkgPath, sym)
	if rej != nil {
		return false
	}
	if _, isFn := obj.(*types.Func); !isFn && strings.Contains(sym, ".") {
		return false
	}
	return true
}
