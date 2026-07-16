package snapshot

import (
	"strings"
	"testing"
)

func TestPatchSingleRename(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"rename","to":"Twice"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Twice"); err != nil {
		t.Fatal(err)
	}
}

func TestPatchStaleGeneration(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double","generation":999,
		"ops":[{"op":"rename","to":"Twice"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.HasPrefix(rej.Reason, "stale generation") {
		t.Fatalf("want stale reject, got %v", err)
	}
}

func TestPatchUnknownOp(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib","sym":"Double",
		"ops":[{"op":"frobnicate"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "unknown op" || len(rej.DidYouMean) == 0 {
		t.Fatalf("want unknown-op reject with catalog suggestions, got %v", err)
	}
}
