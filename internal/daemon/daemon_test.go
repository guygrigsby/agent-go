package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	go func() { done <- handle(server, snap, nil) }()

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
	go func() { done <- handle(server, snap, nil) }()

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

// An exact resend of a just-rejected request escalates: the response gains
// a resent count and a hard imperative instead of letting the loop spin to
// the cap. The smoke episode 20260715-210756 resent one rejected view 23
// times unchanged.
func TestHandleEscalatesIdenticalResend(t *testing.T) {
	dir, err := filepath.Abs("../snapshot/testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot.New(dir)
	breaker := newResendBreaker()
	send := func() map[string]any {
		client, server := net.Pipe()
		done := make(chan bool, 1)
		go func() { done <- handleWithBreaker(server, snap, nil, breaker) }()
		req := protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "proxy.go"}
		if err := json.NewEncoder(client).Encode(req); err != nil {
			t.Fatal(err)
		}
		var res map[string]any
		if err := json.NewDecoder(client).Decode(&res); err != nil {
			t.Fatal(err)
		}
		<-done
		return res
	}
	first := send()
	if first["status"] != "rejected" {
		t.Fatalf("want rejected, got %v", first)
	}
	if _, ok := first["resent"]; ok {
		t.Fatalf("first rejection must not carry resent: %v", first)
	}
	second := send()
	if second["resent"].(float64) != 1 {
		t.Fatalf("second identical send must carry resent=1: %v", second)
	}
	esc, _ := second["escalation"].(string)
	if esc == "" {
		t.Fatalf("second identical send must carry an escalation: %v", second)
	}
	// A successful call must not trip the breaker.
	client, server := net.Pipe()
	done := make(chan bool, 1)
	go func() { done <- handleWithBreaker(server, snap, nil, breaker) }()
	json.NewEncoder(client).Encode(protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Double"})
	var res map[string]any
	json.NewDecoder(client).Decode(&res)
	<-done
	if res["status"] != "ok" || res["resent"] != nil {
		t.Fatalf("ok call polluted by breaker: %v", res)
	}
}

// With a request log open, every handled request appends one JSONL record
// carrying op, outcome, rejection evidence, and latency — the raw material
// for the per-episode counters.
func TestHandleWritesRequestLog(t *testing.T) {
	dir, err := filepath.Abs("../snapshot/testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	rlog, err := openRequestLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot.New(dir)

	send := func(req protocol.Request) {
		client, server := net.Pipe()
		done := make(chan bool, 1)
		go func() { done <- handle(server, snap, rlog) }()
		if err := json.NewEncoder(client).Encode(req); err != nil {
			t.Fatal(err)
		}
		var res map[string]any
		if err := json.NewDecoder(client).Decode(&res); err != nil {
			t.Fatal(err)
		}
		<-done
	}
	send(protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Doub"})
	send(protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Double"})
	send(protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Doub"}) // identical resend
	rlog.Close()

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d:\n%s", len(lines), b)
	}
	var first, second, third map[string]any
	for i, target := range []*map[string]any{&first, &second, &third} {
		if err := json.Unmarshal([]byte(lines[i]), target); err != nil {
			t.Fatal(err)
		}
	}
	if first["op"] != "view" || first["outcome"] != "rejected" ||
		first["reason"] != "symbol not found" || first["repairs"].(float64) < 1 {
		t.Fatalf("first record wrong: %v", first)
	}
	if second["outcome"] != "ok" {
		t.Fatalf("second record wrong: %v", second)
	}
	if first["req_sha"] != third["req_sha"] {
		t.Fatal("identical resend must hash identically")
	}
	if first["req_sha"] == second["req_sha"] {
		t.Fatal("different requests must hash differently")
	}
	if _, ok := first["ms"]; !ok {
		t.Fatalf("record missing latency: %v", first)
	}
}
