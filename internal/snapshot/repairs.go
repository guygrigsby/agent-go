package snapshot

import (
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
