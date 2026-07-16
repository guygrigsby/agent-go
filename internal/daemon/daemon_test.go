package daemon

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/agent-go/internal/protocol"
	"github.com/guygrigsby/agent-go/internal/snapshot"
)

// TestHandlePreservesDidYouMean is a wire-level check that the rejection
// marshaling in handle carries a Reject's DidYouMean through to the client.
// The hand-built response map used to drop it silently for every op.
func TestHandlePreservesDidYouMean(t *testing.T) {
	dir, err := filepath.Abs("../snapshot/testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot.New(dir)
	client, server := net.Pipe()
	done := make(chan bool, 1)
	go func() { done <- handle(server, snap) }()

	req := protocol.Request{Op: "inspect", Pkg: "demo/lib", Sym: "Doubl"}
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.NewDecoder(client).Decode(&res); err != nil {
		t.Fatal(err)
	}
	<-done

	if res["status"] != "rejected" {
		t.Fatalf("want rejected, got %v", res)
	}
	dym, ok := res["did_you_mean"].([]any)
	if !ok || len(dym) == 0 {
		t.Fatalf("want non-empty did_you_mean, got %v", res["did_you_mean"])
	}
	found := false
	for _, s := range dym {
		if s == "Double" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want %q suggested, got %v", "Double", dym)
	}
}

// Same wire-level check for PossibleRepairs: a view addressing miss must
// deliver complete paste-ready calls to the client.
func TestHandlePreservesPossibleRepairs(t *testing.T) {
	dir, err := filepath.Abs("../snapshot/testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot.New(dir)
	client, server := net.Pipe()
	done := make(chan bool, 1)
	go func() { done <- handle(server, snap) }()

	req := protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Doub"}
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.NewDecoder(client).Decode(&res); err != nil {
		t.Fatal(err)
	}
	<-done

	if res["status"] != "rejected" {
		t.Fatalf("want rejected, got %v", res)
	}
	reps, ok := res["possible_repairs"].([]any)
	if !ok || len(reps) == 0 {
		t.Fatalf("want non-empty possible_repairs, got %v", res["possible_repairs"])
	}
	call, ok := reps[0].(map[string]any)["call"].(map[string]any)
	if !ok || call["tool"] != "view" {
		t.Fatalf("want a complete view call, got %v", reps[0])
	}
}
