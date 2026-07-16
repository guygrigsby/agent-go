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

// The tests gate only counts when ground-truth-equivalent code could pass
// it in this environment: four oracle-run rename tasks failed scoped tests
// on a perfect change (docker deps, parent rot). A failing baseline makes
// the gate vacuous, not the episode failed.
func TestPassRule(t *testing.T) {
	cases := []struct {
		pred, tc, tests, baseline, want bool
	}{
		{true, true, true, true, true},    // everything green
		{true, true, false, true, false},  // tests fail where baseline passes: real failure
		{true, true, false, false, true},  // tests fail but baseline also fails: gate vacuous
		{false, true, true, true, false},  // predicate is never waived
		{true, false, false, false, false}, // typecheck is never waived
	}
	for i, c := range cases {
		if got := passRule(c.pred, c.tc, c.tests, c.baseline); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}
