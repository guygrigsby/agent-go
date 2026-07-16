package snapshot

import "testing"

// Every repair on a view rejection must be a complete view call that
// succeeds when executed verbatim.
func execViewRepairs(t *testing.T, s *Snapshot, rej *Reject) {
	t.Helper()
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	for _, r := range rej.PossibleRepairs {
		if r.Call["tool"] != "view" {
			t.Fatalf("repair tool = %v, want view", r.Call["tool"])
		}
		args, ok := r.Call["args"].(map[string]any)
		if !ok {
			t.Fatalf("repair args missing: %+v", r.Call)
		}
		res, err := s.View(args["pkg"].(string), args["sym"].(string))
		if err != nil {
			t.Fatalf("repair %v rejected: %v", r.Call, err)
		}
		if res["status"] != "ok" {
			t.Fatalf("repair %v: %v", r.Call, res)
		}
	}
}

func TestViewSymbolMissRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Doub")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
}

func TestViewPackageMissRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo.lib", "Double")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
}

func TestViewReceiverMissRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Stor.Put")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
}

func TestViewMethodMissRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Store.Putt")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
}

// Viewing a field redirects to the containing type; the repair is the
// view call for that type, executable verbatim.
func TestViewFieldRepairViewsOwner(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Store.n")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
	if sym := rej.PossibleRepairs[0].Call["args"].(map[string]any)["sym"]; sym != "Store" {
		t.Fatalf("repair sym = %v, want Store", sym)
	}
}
