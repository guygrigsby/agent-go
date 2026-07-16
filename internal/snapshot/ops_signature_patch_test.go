package snapshot

import (
	"strings"
	"testing"
)

// set_signature adds a leading param with a default at every call site,
// including a spread site add_param cannot handle.
func TestSetSignatureInsertBeforeVariadic(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[{"op":"set_signature","sym":"Fetch",
		"signature":"(ctx context.Context, a int, b string, rest ...int) int",
		"defaults":{"ctx":"context.Background()"}}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, err := s.View("demo/sig", "SpreadFetch")
	if err != nil {
		t.Fatal(err)
	}
	if text := out["text"].(string); !strings.Contains(text, "Fetch(context.Background(), 1, \"x\", nums...)") {
		t.Fatalf("spread site not rewritten:\n%s", text)
	}
	out, _ = s.View("demo/sig", "UseFetch")
	if text := out["text"].(string); !strings.Contains(text, "Fetch(context.Background(), 1, \"x\", 2, 3)") {
		t.Fatalf("plain site not rewritten:\n%s", text)
	}
}

// Dropping a parameter drops its argument everywhere.
func TestSetSignatureDropParam(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[{"op":"set_signature","sym":"Fetch",
		"signature":"(a int, rest ...int) int"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, _ := s.View("demo/sig", "UseFetch")
	if text := out["text"].(string); !strings.Contains(text, "Fetch(1, 2, 3)") {
		t.Fatalf("dropped arg still passed:\n%s", text)
	}
}

// Dropping a parameter the body still uses rejects at end-of-list
// typecheck and changes nothing; pairing the same drop with a set_body
// that repairs the body in the same patch is accepted. This is the
// "call sites and body repaired in the same patch" contract.
func TestSetSignatureBodyUseRejectsAloneAcceptsPaired(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[{"op":"set_signature","sym":"Fetch",
		"signature":"(b string, rest ...int) int"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" {
		t.Fatalf("want typecheck reject (body still uses a), got %v", err)
	}
	if out, _ := s.View("demo/sig", "Fetch"); !strings.Contains(out["text"].(string), "a int") {
		t.Fatal("rejected patch must change nothing")
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[{"op":"set_signature","sym":"Fetch",
		"signature":"(b string, rest ...int) int"},
		{"op":"set_body","sym":"Fetch","body":"n := len(b)\nfor _, r := range rest {\n n += r\n}\nreturn n"}]}`))
	if err != nil {
		t.Fatalf("paired repair patch rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
}
