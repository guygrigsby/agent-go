package snapshot

import (
	"strings"
	"testing"
)

func TestViewHandles(t *testing.T) {
	s := demo(t)
	res, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	text := res["text"].(string)
	if !strings.Contains(text, "n1:") || !strings.Contains(text, "n3:") {
		t.Fatalf("missing handles:\n%s", text)
	}
	if res["generation"].(int64) == 0 {
		t.Fatal("view must carry generation")
	}
	// Handles are stable across identical views.
	res2, _ := s.View("demo/lib", "UseHelper")
	if res2["text"] != text {
		t.Fatal("view not deterministic")
	}
}

func TestViewNonFunction(t *testing.T) {
	s := demo(t)
	res, err := s.View("demo/lib", "Limit")
	if err != nil {
		t.Fatal(err)
	}
	// Non-function decls render without handles.
	if !strings.Contains(res["text"].(string), "Limit") {
		t.Fatalf("got %v", res)
	}
}

func TestViewFieldRejectsWithOwner(t *testing.T) {
	s := demo(t)
	_, err := s.View("demo/lib", "Store.n")
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason, "containing type") {
		t.Fatalf("want containing-type reject, got %v", err)
	}
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "Store" {
		t.Fatalf("want owner suggestion, got %v", rej.DidYouMean)
	}
}
