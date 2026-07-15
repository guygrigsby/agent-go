package snapshot

import (
	"fmt"

	"golang.org/x/tools/go/packages"
)

// Generations are per-package counters, monotonic for the daemon's life.
// Any splice or reload bumps every package it touched; a view or patch
// built against an older number is stale. Package granularity is coarser
// than the spec's per-declaration ideal but strictly safe.
// ponytail: refine to per-declaration only if re-view round-trips show up
// in bench time-to-green.
func (s *Snapshot) generation(pkgPath, sym string) int64 {
	return s.gens[pkgPath]
}

func (s *Snapshot) bumpGenerations(dirty []*packages.Package) {
	if s.gens == nil {
		s.gens = map[string]int64{}
	}
	seen := map[string]bool{}
	for _, p := range dirty {
		if !seen[p.PkgPath] {
			seen[p.PkgPath] = true
			s.gens[p.PkgPath]++
		}
	}
}

func (s *Snapshot) checkGeneration(pkgPath, sym string, want int64) *Reject {
	if want == 0 {
		return nil
	}
	cur := s.generation(pkgPath, sym)
	if cur == want {
		return nil
	}
	return &Reject{Reason: "stale generation: re-view",
		Detail: fmt.Sprintf("%s is at generation %d, patch was built against %d", pkgPath, cur, want)}
}
