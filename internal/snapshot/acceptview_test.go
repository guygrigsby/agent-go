package snapshot

import (
	"strings"
	"testing"
)

// requireView pulls the embedded view out of an accepted mutation response
// and proves it is byte-identical to what a follow-up View call returns —
// the whole point of embedding it is that the caller can skip that call.
func requireView(t *testing.T, s *Snapshot, res map[string]any, pkg, sym string) map[string]any {
	t.Helper()
	if _, omitted := res["views_omitted"]; omitted {
		t.Fatalf("single-decl accept must not omit the view: %v", res)
	}
	v, ok := res["view"].(map[string]any)
	if !ok {
		t.Fatalf("accepted response missing view: %v", res)
	}
	fresh, err := s.View(pkg, sym)
	if err != nil {
		t.Fatal(err)
	}
	if v["text"] != fresh["text"] || v["nodes"] != fresh["nodes"] || v["generation"] != fresh["generation"] {
		t.Fatalf("embedded view differs from View:\n%v\nvs\n%v", v, fresh)
	}
	if v["generation"] != res["generation"] {
		t.Fatalf("view generation %v != response generation %v", v["generation"], res["generation"])
	}
	return v
}

func TestSetBodyAcceptView(t *testing.T) {
	s := demo(t)
	res, err := s.SetBody("demo/lib", "Double", "return v + v")
	if err != nil {
		t.Fatal(err)
	}
	v := requireView(t, s, res, "demo/lib", "Double")
	if !strings.Contains(v["text"].(string), "v + v") {
		t.Fatalf("view text is stale:\n%s", v["text"])
	}
}

func TestRenameAcceptViewNewName(t *testing.T) {
	s := demo(t)
	res, err := s.Rename("demo/lib", "Double", "Twice")
	if err != nil {
		t.Fatal(err)
	}
	// The viewed symbol is the NEW name.
	v := requireView(t, s, res, "demo/lib", "Twice")
	if !strings.Contains(v["text"].(string), "func Twice") {
		t.Fatalf("view text does not show the renamed decl:\n%s", v["text"])
	}
}

func TestAddParamAcceptView(t *testing.T) {
	s := demo(t)
	res, err := s.AddParam("demo/lib", "Double", "scale", "int", "1")
	if err != nil {
		t.Fatal(err)
	}
	v := requireView(t, s, res, "demo/lib", "Double")
	if !strings.Contains(v["text"].(string), "scale int") {
		t.Fatalf("view text missing new param:\n%s", v["text"])
	}
}

func TestUpsertDeclAcceptView(t *testing.T) {
	s := demo(t)
	res, err := s.UpsertDecl("demo/lib", "func Triple(v int) int { return v * 3 }")
	if err != nil {
		t.Fatal(err)
	}
	v := requireView(t, s, res, "demo/lib", "Triple")
	if !strings.Contains(v["text"].(string), "v * 3") {
		t.Fatalf("view text missing new decl:\n%s", v["text"])
	}
}

func TestPatchDeclOpAcceptView(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"set_body","body":"return v + v"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	v := requireView(t, s, res, "demo/lib", "Double")
	if !strings.Contains(v["text"].(string), "v + v") {
		t.Fatalf("view text is stale:\n%s", v["text"])
	}
}

// Several statement ops still touch exactly one declaration — the
// envelope's own function — so the accept response carries its view.
func TestPatchStmtOpsAcceptView(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"UseHelper",
		"ops":[{"op":"add_assign","at":"n1","where":"after","lhs":"extra","rhs":"h(1)","define":true},
		       {"op":"add_assign","at":"n3","where":"before","lhs":"_","rhs":"extra"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	v := requireView(t, s, res, "demo/lib", "UseHelper")
	text := v["text"].(string)
	if !strings.Contains(text, "extra := h(1)") || !strings.Contains(text, "n1:") {
		t.Fatalf("view text missing fresh handles/edit:\n%s", text)
	}
}

// A patch touching several declarations has no single "the touched
// declaration": the view key is omitted and views_omitted says why.
func TestPatchMultiDeclOmitsView(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"rename","sym":"Double","to":"Twice"},
		       {"op":"rename","sym":"Tail","to":"Rear"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, ok := res["view"]; ok {
		t.Fatalf("multi-decl patch must omit view: %v", res)
	}
	reason, ok := res["views_omitted"].(string)
	if !ok || reason == "" {
		t.Fatalf("want views_omitted reason, got %v", res)
	}
}

// delete_decl leaves nothing to view; the accept response says so instead
// of carrying a view of a declaration that no longer exists.
func TestPatchDeleteDeclOmitsView(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Triple(v int) int { return v * 3 }"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"delete_decl","sym":"Triple"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, ok := res["view"]; ok {
		t.Fatalf("deleted decl must omit view: %v", res)
	}
	if res["views_omitted"] != "declaration was deleted" {
		t.Fatalf("want deleted-decl reason, got %v", res["views_omitted"])
	}
}
