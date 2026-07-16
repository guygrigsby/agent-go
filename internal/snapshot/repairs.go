package snapshot

import (
	"encoding/json"
	"go/types"
	"maps"
	"strings"
)

const maxRepairs = 3

// viewCall builds the complete view invocation for an address.
func viewCall(pkgPath, sym string) map[string]any {
	return map[string]any{"tool": "view", "args": map[string]any{"pkg": pkgPath, "sym": sym}}
}

// addressingReason reports whether a reject's did_you_mean candidates are
// substitutions for the missed part of a pkg/sym address.
func addressingReason(r string) bool {
	switch r {
	case "symbol not found", "method or field not found",
		"receiver type not found", "package not found":
		return true
	}
	return false
}

// substituteAddress applies one candidate to the part of the address the
// reject's reason says was missed.
func substituteAddress(reason, pkgPath, sym, cand string) (string, string) {
	switch reason {
	case "package not found":
		return cand, sym
	case "receiver type not found":
		_, name, _ := strings.Cut(sym, ".")
		return pkgPath, cand + "." + name
	default:
		return pkgPath, cand
	}
}

// addressRepairs fills rej.PossibleRepairs with complete calls built by
// substituting the missed part of the address with each did_you_mean
// candidate. Only substitutions the accepting predicate admits survive: a
// repair that rejects when pasted back is worse than none.
func addressRepairs(rej *Reject, pkgPath, sym string,
	build func(pkg, sym string) map[string]any, accept func(pkg, sym string) bool) {
	for _, c := range rej.DidYouMean {
		pkg, symc := substituteAddress(rej.Reason, pkgPath, sym, c)
		if !accept(pkg, symc) {
			continue
		}
		rej.PossibleRepairs = append(rej.PossibleRepairs,
			Repair{Why: pkg + "." + symc + " resolves", Call: build(pkg, symc)})
		if len(rej.PossibleRepairs) == maxRepairs {
			return
		}
	}
}

// viewRepairs builds complete view calls for a view addressing miss.
// Caller holds mu.
func (s *Snapshot) viewRepairs(rej *Reject, pkgPath, sym string) {
	addressRepairs(rej, pkgPath, sym, viewCall, s.viewable)
}

// resolves reports whether the address names any object.
func (s *Snapshot) resolves(pkg, sym string) bool {
	_, _, miss := s.findObject(pkg, sym)
	return miss == nil
}

// resolvesToFunc reports whether the address names a function or method.
func (s *Snapshot) resolvesToFunc(pkg, sym string) bool {
	_, obj, miss := s.findObject(pkg, sym)
	if miss != nil {
		return false
	}
	_, ok := obj.(*types.Func)
	return ok
}

// queryRepairs builds complete query calls of the same kind for a query
// addressing miss. Takes mu itself: Query's sub-handlers have already
// released it by the time the reject surfaces.
func (s *Snapshot) queryRepairs(rej *Reject, kind, pkgPath, sym string) {
	if !addressingReason(rej.Reason) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Call-graph kinds only accept functions; a resolving candidate of
	// the wrong kind would still reject.
	accept := s.resolves
	if kind == "callers" || kind == "callees" {
		accept = s.resolvesToFunc
	}
	addressRepairs(rej, pkgPath, sym,
		func(pkg, sym string) map[string]any {
			return map[string]any{"tool": "query",
				"args": map[string]any{"kind": kind, "pkg": pkg, "sym": sym}}
		}, accept)
}

// sugarRepairs builds the corrected single-op call for an addressing miss
// on a sugar mutation: the full argument list is echoed with the missed
// address part substituted. accept gates candidates on what the tool
// itself requires. Caller holds mu.
func (s *Snapshot) sugarRepairs(rej *Reject, tool string, args map[string]any,
	accept func(pkg, sym string) bool) {
	if !addressingReason(rej.Reason) {
		return
	}
	pkg, _ := args["pkg"].(string)
	sym, _ := args["sym"].(string)
	addressRepairs(rej, pkg, sym, func(p, y string) map[string]any {
		out := make(map[string]any, len(args))
		maps.Copy(out, args)
		out["pkg"], out["sym"] = p, y
		return map[string]any{"tool": tool, "args": out}
	}, accept)
}

// repairField maps op-reject reasons whose did_you_mean candidates are
// values for one specific op argument, so the repair is the same patch
// with that argument substituted.
var repairField = map[string]string{
	"unknown insertion point":       "where",
	"unknown wrap_stmts with value": "with",
}

// patchOpRepairs adds the recovery call for op-level rejects. A missed
// handle means the caller's handle table is stale or invented, and the
// literal next call is the view that rebuilds it; a bad keyword argument
// resends the whole patch with the keyword substituted. i is the 0-based
// index of the rejected op. Caller holds mu.
func (s *Snapshot) patchOpRepairs(rej *Reject, env patchEnvelope, i int) {
	if rej.Reason == "unknown handle" {
		if env.Sym != "" && s.viewable(env.Pkg, env.Sym) {
			rej.PossibleRepairs = append(rej.PossibleRepairs,
				Repair{Why: "rebuilds the handle table for " + env.Pkg + "." + env.Sym,
					Call: viewCall(env.Pkg, env.Sym)})
		}
		return
	}
	if field := repairField[rej.Reason]; field != "" {
		for _, c := range rej.DidYouMean {
			call, ok := patchCall(env, i, field, c)
			if !ok {
				return
			}
			rej.PossibleRepairs = append(rej.PossibleRepairs,
				Repair{Why: field + " " + c + " is valid", Call: call})
			if len(rej.PossibleRepairs) == maxRepairs {
				return
			}
		}
		return
	}
	if addressingReason(rej.Reason) {
		s.patchAddressRepairs(rej, env, i)
		return
	}
	// A missing required argument can't be filled in; the correct next
	// call is the catalog entry that shows the op's full schema.
	// ponytail: "required" substring is the detection; every shape reject
	// in the tree phrases itself that way, add a marker field if one stops.
	if strings.Contains(rej.Reason, "required") {
		var n opName
		json.Unmarshal(env.Ops[i], &n)
		rej.PossibleRepairs = append(rej.PossibleRepairs,
			Repair{Why: "the catalog shows " + n.Op + "'s full schema",
				Call: map[string]any{"tool": "help", "args": map[string]any{}}})
	}
}

// patchAddressRepairs handles addressing misses inside an op: the repair
// is the whole patch with the op's pkg or sym substituted by a candidate
// that resolves. Caller holds mu.
func (s *Snapshot) patchAddressRepairs(rej *Reject, env patchEnvelope, i int) {
	if !addressingReason(rej.Reason) {
		return
	}
	// The op's effective addressing, with envelope defaults applied.
	var addr struct {
		Pkg string `json:"pkg"`
		Sym string `json:"sym"`
	}
	if json.Unmarshal(env.Ops[i], &addr) != nil {
		return
	}
	pkg, sym := addr.Pkg, addr.Sym
	if pkg == "" {
		pkg = env.Pkg
	}
	if sym == "" {
		sym = env.Sym
	}
	for _, c := range rej.DidYouMean {
		pkgc, symc := substituteAddress(rej.Reason, pkg, sym, c)
		field, val := "sym", symc
		if rej.Reason == "package not found" {
			field, val = "pkg", pkgc
		}
		if _, _, miss := s.findObject(pkgc, symc); miss != nil {
			continue
		}
		call, ok := patchCall(env, i, field, val)
		if !ok {
			return
		}
		rej.PossibleRepairs = append(rej.PossibleRepairs,
			Repair{Why: pkgc + "." + symc + " resolves", Call: call})
		if len(rej.PossibleRepairs) == maxRepairs {
			return
		}
	}
}

// patchCall rebuilds the complete patch invocation from a parsed envelope
// with op index i's field replaced by cand. Only envelope fields are
// echoed, so transport extras in the original bytes are dropped.
func patchCall(env patchEnvelope, i int, field, cand string) (map[string]any, bool) {
	ops := make([]any, len(env.Ops))
	for j, raw := range env.Ops {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		ops[j] = m
	}
	ops[i].(map[string]any)[field] = cand
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
