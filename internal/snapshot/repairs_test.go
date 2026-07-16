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

// A missed handle means the caller's handle table is stale or invented;
// the repair is the view call that rebuilds it, executable verbatim.
func TestPatchUnknownHandleRepairIsView(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n99","where":"after","lhs":"x","rhs":"1","define":true}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown handle" {
		t.Fatalf("want unknown-handle Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
	args := rej.PossibleRepairs[0].Call["args"].(map[string]any)
	if args["sym"] != "UseHelper" {
		t.Fatalf("repair views %v, want UseHelper", args)
	}
}

// An envelope sym miss on a statement-op patch repairs like any other
// addressing miss: complete view calls for the near-miss candidates.
func TestPatchEnvelopeSymMissRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelp",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"x","rhs":"1","define":true}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	execViewRepairs(t, s, rej)
}

// A bad keyword argument (where/with) repairs with the whole patch resent,
// the keyword substituted; the first repair is accepted when executed.
func TestPatchBadWhereRepairIsCorrectedPatch(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"behind","lhs":"_","rhs":"1"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown insertion point" {
		t.Fatalf("want insertion-point Reject, got %v", err)
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
}

// A decl op naming a missing symbol repairs with the whole patch resent,
// the sym substituted with a resolving candidate; executed verbatim it is
// accepted.
func TestPatchOpSymMissRepairIsCorrectedPatch(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Doub","to":"Twice"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol not found" {
		t.Fatalf("want symbol-not-found Reject, got %v", err)
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

// A query addressing miss repairs with complete query calls of the same
// kind, executable verbatim.
func TestQuerySymMissRepairs(t *testing.T) {
	s := demo(t)
	for _, kind := range []string{"inspect", "refs", "callers"} {
		_, err := s.Query(kind, "demo/lib", "Doub", "")
		rej, ok := err.(*Reject)
		if !ok {
			t.Fatalf("%s: want Reject, got %v", kind, err)
		}
		if len(rej.PossibleRepairs) == 0 {
			t.Fatalf("%s: reject carries no repairs: %+v", kind, rej)
		}
		for _, r := range rej.PossibleRepairs {
			if r.Call["tool"] != "query" {
				t.Fatalf("%s: repair tool = %v, want query", kind, r.Call["tool"])
			}
			args := r.Call["args"].(map[string]any)
			if args["kind"] != kind {
				t.Fatalf("%s: repair kind = %v", kind, args["kind"])
			}
			res, err := s.Query(kind, args["pkg"].(string), args["sym"].(string), "")
			if err != nil {
				t.Fatalf("%s: repair %v rejected: %v", kind, r.Call, err)
			}
			if res["status"] != "ok" {
				t.Fatalf("%s: repair %v: %v", kind, r.Call, res)
			}
		}
	}
}

// A sugar mutation naming a missing symbol repairs with the corrected
// call, all arguments echoed; executed verbatim it is accepted.
func TestRenameSymMissRepair(t *testing.T) {
	s := demo(t)
	_, err := s.Rename("demo/lib", "Doub", "Twice")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	r := rej.PossibleRepairs[0]
	if r.Call["tool"] != "rename" {
		t.Fatalf("repair tool = %v, want rename", r.Call["tool"])
	}
	args := r.Call["args"].(map[string]any)
	if args["to"] != "Twice" {
		t.Fatalf("repair dropped args: %v", args)
	}
	res, err := s.Rename(args["pkg"].(string), args["sym"].(string), args["to"].(string))
	if err != nil {
		t.Fatalf("repair rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("repair not accepted: %v", res)
	}
}

func TestSetBodySymMissRepair(t *testing.T) {
	s := demo(t)
	_, err := s.SetBody("demo/lib", "Doub", "return v * 3")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	args := rej.PossibleRepairs[0].Call["args"].(map[string]any)
	res, err := s.SetBody(args["pkg"].(string), args["sym"].(string), args["body"].(string))
	if err != nil {
		t.Fatalf("repair rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("repair not accepted: %v", res)
	}
}

func TestAddParamSymMissRepair(t *testing.T) {
	s := demo(t)
	_, err := s.AddParam("demo/lib", "Doub", "n", "int", "0")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	args := rej.PossibleRepairs[0].Call["args"].(map[string]any)
	res, err := s.AddParam(args["pkg"].(string), args["sym"].(string),
		args["name"].(string), args["type"].(string), args["default"].(string))
	if err != nil {
		t.Fatalf("repair rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("repair not accepted: %v", res)
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
