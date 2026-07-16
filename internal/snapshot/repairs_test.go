package snapshot

import (
	"encoding/json"
	"testing"
)

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

// A stale-generation patch reject repairs with the view call that
// refreshes the handle, executable verbatim.
func TestPatchStaleGenerationRepairIsView(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double","generation":999,
		"ops":[{"op":"rename","to":"Twice"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
	args := rej.PossibleRepairs[0].Call["args"].(map[string]any)
	if args["pkg"] != "demo/lib" || args["sym"] != "Double" {
		t.Fatalf("repair views %v", args)
	}
}

// An unknown-op patch reject repairs with the whole patch resent, the op
// name corrected and every other field preserved; the repair is accepted
// when executed verbatim.
func TestPatchUnknownOpRepairIsCorrectedPatch(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"renam","to":"Twice"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	r := rej.PossibleRepairs[0]
	if r.Call["tool"] != "patch" {
		t.Fatalf("repair tool = %v, want patch", r.Call["tool"])
	}
	raw, err := json.Marshal(r.Call["args"])
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch(raw)
	if err != nil {
		t.Fatalf("repair rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("repair not accepted: %v", res)
	}
	if _, err := s.Inspect("demo/lib", "Twice"); err != nil {
		t.Fatal("corrected rename did not land:", err)
	}
}

// A catalog dump is not a near-miss: when nothing matches the op name,
// no repair is invented.
func TestPatchUnknownOpNoRepairWithoutNearMiss(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"frobnicate"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) != 0 {
		t.Fatalf("invented repairs for a no-match miss: %+v", rej.PossibleRepairs)
	}
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
