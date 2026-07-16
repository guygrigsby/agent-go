package bench

import "testing"

// Goal predicates dispatch on Manifest.Kind; empty means rename (the
// original task family). An unknown kind has no predicate and the episode
// must record itself unscorable rather than silently passing.
func TestPredicateDispatch(t *testing.T) {
	for _, kind := range []string{"", "rename", "add-param"} {
		if predicateFor(kind) == nil {
			t.Fatalf("no predicate for kind %q", kind)
		}
	}
	if predicateFor("teleport") != nil {
		t.Fatal("unknown kind must have no predicate")
	}
}
