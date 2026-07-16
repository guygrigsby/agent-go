package snapshot

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every repair on a view rejection must be a complete call that succeeds
// when executed verbatim — a view call, or a discovery query (inspect,
// search) when substitution had nothing.
func execViewRepairs(t *testing.T, s *Snapshot, rej *Reject) {
	t.Helper()
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	for _, r := range rej.PossibleRepairs {
		args, ok := r.Call["args"].(map[string]any)
		if !ok {
			t.Fatalf("repair args missing: %+v", r.Call)
		}
		var res map[string]any
		var err error
		switch r.Call["tool"] {
		case "view":
			res, err = s.View(args["pkg"].(string), args["sym"].(string))
		case "query":
			pkg, _ := args["pkg"].(string)
			sym, _ := args["sym"].(string)
			q, _ := args["q"].(string)
			res, err = s.Query(args["kind"].(string), pkg, sym, q, 0)
		default:
			t.Fatalf("unexpected repair tool %v", r.Call["tool"])
		}
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

// A missing required argument cannot be invented; the repair is the help
// call whose catalog entry shows the op's full schema, executable verbatim.
func TestPatchShapeErrorRepairIsHelp(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_param","sym":"Double","name":"n"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	r := rej.PossibleRepairs[0]
	if r.Call["tool"] != "help" {
		t.Fatalf("repair tool = %v, want help", r.Call["tool"])
	}
	res, err := s.Help()
	if err != nil || res["status"] != "ok" {
		t.Fatalf("help repair not executable: %v %v", res, err)
	}
}

// An undefined identifier in a typecheck reject has no mechanical fix,
// but it has a mechanical next step: the search call that locates it.
func TestPatchTypecheckUndefinedRepairIsSearch(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"set_body","sym":"Double","body":"return Helpr(v)"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" {
		t.Fatalf("want typecheck Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("reject carries no repairs: %+v", rej)
	}
	r := rej.PossibleRepairs[0]
	args, _ := r.Call["args"].(map[string]any)
	if r.Call["tool"] != "query" || args["kind"] != "search" || args["q"] != "Helpr" {
		t.Fatalf("want search repair for Helpr, got %v", r.Call)
	}
	res, err := s.Query("search", "", "", args["q"].(string), 0)
	if err != nil || res["status"] != "ok" {
		t.Fatalf("search repair not executable: %v %v", res, err)
	}
}

// add_param whose parameter type needs an import the file lacks must add
// the import, not reject with "undefined". Oracle finding: task
// boundary_b4b95e0f rejected adding ctx context.Context this way.
func TestAddParamManagesImports(t *testing.T) {
	s := demo(t)
	res, err := s.AddParam("demo/lib", "Double", "ctx", "context.Context", "context.Background()")
	if err != nil {
		t.Fatalf("add_param rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, err := s.View("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	if text := out["text"].(string); !strings.Contains(text, "ctx context.Context") {
		t.Fatalf("param missing:\n%s", text)
	}
}

// add_param on a variadic function inserts the default before the
// variadic tail at every call site — including spread sites, which used
// to reject (oracle blockers boundary_bf1486f7, eb61ac63).
func TestAddParamVariadicSpreadSites(t *testing.T) {
	s := demo(t)
	res, err := s.AddParam("demo/sig", "Fetch", "scale", "int", "1")
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, _ := s.View("demo/sig", "SpreadFetch")
	if text := out["text"].(string); !strings.Contains(text, `Fetch(1, "x", 1, nums...)`) {
		t.Fatalf("spread site wrong:\n%s", text)
	}
	out, _ = s.View("demo/sig", "UseFetch")
	if text := out["text"].(string); !strings.Contains(text, `Fetch(1, "x", 1, 2, 3)`) {
		t.Fatalf("value-variadic site wrong:\n%s", text)
	}
}

// Grouped parameters "(a, b int)" count as two arguments; the call-site
// insertion point must count names, not fields (oracle regression:
// boundary_b4b95e0f "result does not parse").
func TestAddParamGroupedParams(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/sig",
		"func Pair(a, b int) int { return a + b }"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDecl("demo/sig",
		"func UsePair() int { return Pair(1, 2) }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.AddParam("demo/sig", "Pair", "c", "int", "0")
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, _ := s.View("demo/sig", "UsePair")
	if text := out["text"].(string); !strings.Contains(text, "Pair(1, 2, 0)") {
		t.Fatalf("call site wrong:\n%s", text)
	}
}

// A multiline parameter list ends with a trailing comma before the
// closing paren; appending ", name type" there produces ", ," (oracle:
// boundary_b4b95e0f NewService, "result does not parse").
func TestAddParamTrailingCommaParams(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/sig",
		"func Wide(\n\ta int,\n\tb string,\n) int {\n\treturn a\n}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDecl("demo/sig",
		"func UseWide() int { return Wide(1, \"x\") }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.AddParam("demo/sig", "Wide", "c", "int", "0")
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, _ := s.View("demo/sig", "UseWide")
	if text := out["text"].(string); !strings.Contains(text, `Wide(1, "x", 0)`) {
		t.Fatalf("call site wrong:\n%s", text)
	}
}

// An op carrying another op's arguments (GLM episode 20260715-212439 sent
// set_body with at/where/exprs and no body) must reject at the shape
// layer with the help repair — not reach the splice layer and surface as
// "stale offset".
func TestPatchForeignArgsRejectAtShape(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"set_body","at":"n1","where":"before","exprs":["x := 1"]}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if rej.Reason == "stale offset" || rej.Reason == "patch does not typecheck" {
		t.Fatalf("garbage op reached the splice layer: %v", rej.Reason)
	}
	if !strings.Contains(rej.Reason+rej.Detail, `"at"`) {
		t.Fatalf("reject must name the foreign argument: %v %v", rej.Reason, rej.Detail)
	}
	if len(rej.PossibleRepairs) == 0 || rej.PossibleRepairs[0].Call["tool"] != "help" {
		t.Fatalf("shape reject must carry the help repair: %+v", rej.PossibleRepairs)
	}
}

// Floor-model addressing shapes (qwen3.5-9b smoke, run 20260716): a
// package miss with no sym, or no pkg with a sym, cannot repair by
// substitution — the mechanical next call is search.
func TestAddressMissWithoutSymRepairsToSearch(t *testing.T) {
	s := demo(t)
	for _, c := range []struct{ pkg, sym, q string }{
		{"lib", "", "lib"},             // bare package name, no sym
		{"", "UseHelper", "UseHelper"}, // sym with no pkg
	} {
		_, err := s.Query("refs", c.pkg, c.sym, "", 0)
		rej, ok := err.(*Reject)
		if !ok {
			t.Fatalf("%q/%q: want Reject, got %v", c.pkg, c.sym, err)
		}
		if len(rej.PossibleRepairs) == 0 {
			t.Fatalf("%q/%q: no repairs: %+v", c.pkg, c.sym, rej)
		}
		r := rej.PossibleRepairs[0]
		args, _ := r.Call["args"].(map[string]any)
		if r.Call["tool"] != "query" || args["kind"] != "search" || args["q"] != c.q {
			t.Fatalf("%q/%q: want search repair for %q, got %v", c.pkg, c.sym, c.q, r.Call)
		}
	}
}

// inspect on a type lists its method set — the discovery move GLM lacked
// on vault_cfff8d42 (40 method-not-found rejects hunting an unexported
// method of an unexported type).
func TestInspectTypeListsMethods(t *testing.T) {
	s := demo(t)
	res, err := s.Inspect("demo/lib", "Store")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res["methods"])
	var methods []struct {
		Name      string `json:"name"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(raw, &methods); err != nil || len(methods) == 0 {
		t.Fatalf("inspect of a type must list methods, got %v (%v)", res["methods"], err)
	}
	found := false
	for _, m := range methods {
		if m.Name == "Put" && strings.Contains(m.Signature, "int") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Store.Put missing from methods: %+v", methods)
	}
	// Non-types don't grow the key.
	res, err = s.Inspect("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res["methods"]; ok {
		t.Fatal("functions must not carry a methods list")
	}
}

// A method-or-field miss now repairs to the inspect call that lists the
// receiver's methods.
func TestMethodMissRepairsToInspect(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Store.Nonexistent")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("no repairs: %+v", rej)
	}
	last := rej.PossibleRepairs[len(rej.PossibleRepairs)-1]
	args, _ := last.Call["args"].(map[string]any)
	if last.Call["tool"] != "query" || args["kind"] != "inspect" || args["sym"] != "Store" {
		t.Fatalf("want inspect-the-type repair, got %v", last.Call)
	}
}

// A synthetic closure name (gopls's funcN convention, resent 30 times by
// GLM) gets the redirect: anonymous functions are not addressable.
func TestSyntheticClosureNameRedirect(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Store.func_1")
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if !strings.Contains(rej.Detail, "anonymous") {
		t.Fatalf("reject must explain anonymous functions are unaddressable: %v %v", rej.Reason, rej.Detail)
	}
	if len(rej.PossibleRepairs) == 0 {
		t.Fatalf("no repairs: %+v", rej)
	}
	args, _ := rej.PossibleRepairs[0].Call["args"].(map[string]any)
	if args["kind"] != "inspect" || args["sym"] != "Store" {
		t.Fatalf("want inspect-the-receiver repair, got %v", rej.PossibleRepairs[0].Call)
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
		_, err := s.Query(kind, "demo/lib", "Doub", "", 0)
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
			res, err := s.Query(kind, args["pkg"].(string), args["sym"].(string), "", 0)
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

// A file name (or file:line) passed as a sym is a raw-mode habit with no
// candidate substitution; the repair is the search call that turns the
// file's base name into real symbol addresses.
func TestViewFilenameSymRepairIsSearch(t *testing.T) {
	s := demo(t)
	for _, sym := range []string{"lib.go", "lib.go:9"} {
		_, err := s.View("demo/lib", sym)
		rej, ok := err.(*Reject)
		if !ok {
			t.Fatalf("%s: want Reject, got %v", sym, err)
		}
		if len(rej.PossibleRepairs) == 0 {
			t.Fatalf("%s: reject carries no repairs: %+v", sym, rej)
		}
		r := rej.PossibleRepairs[0]
		args, _ := r.Call["args"].(map[string]any)
		if r.Call["tool"] != "query" || args["kind"] != "search" || args["q"] != "lib" {
			t.Fatalf("%s: want search repair for lib, got %v", sym, r.Call)
		}
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
