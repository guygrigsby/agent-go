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

// demoDir copies the shared demo fixture into a t.TempDir so mutation ops
// can run against a throwaway workspace, mirroring snapshot_test.go's
// demo() helper but returning the directory for snapshot.New here.
func demoDir(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs("../snapshot/testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	err = filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestHandleEveryOp drives every case of handle's op switch through the
// wire against one snapshot of a temp copy of the demo fixture: each op
// must route to its handler (checked by a response key only that handler
// emits), return its expected status, and never produce a transport error.
// Ordering matters: reads and the test run come first, then the mutations,
// each targeting state the previous steps left behind.
func TestHandleEveryOp(t *testing.T) {
	snap := snapshot.New(demoDir(t))
	send := func(req protocol.Request) (map[string]any, bool) {
		t.Helper()
		client, server := net.Pipe()
		done := make(chan bool, 1)
		go func() { done <- handle(server, snap, nil) }()
		if err := json.NewEncoder(client).Encode(req); err != nil {
			t.Fatalf("%s: encode: %v", req.Op, err)
		}
		var res map[string]any
		if err := json.NewDecoder(client).Decode(&res); err != nil {
			t.Fatalf("%s: transport error decoding response: %v", req.Op, err)
		}
		return res, <-done
	}

	steps := []struct {
		name   string
		req    protocol.Request
		status string
		key    string // response key only the routed handler emits
		stop   bool
	}{
		{"status", protocol.Request{Op: "status"}, "ok", "packages", false},
		{"help", protocol.Request{Op: "help"}, "ok", "ops", false},
		{"inspect", protocol.Request{Op: "inspect", Pkg: "demo/lib", Sym: "Double"}, "ok", "kind", false},
		{"view", protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Double"}, "ok", "text", false},
		{"refs", protocol.Request{Op: "refs", Pkg: "demo/lib", Sym: "Double"}, "ok", "refs", false},
		{"search", protocol.Request{Op: "search", Sym: "Doub"}, "ok", "symbols", false},
		{"query search", protocol.Request{Op: "query", Kind: "search", Sym: "Doub"}, "ok", "symbols", false},
		{"query inspect", protocol.Request{Op: "query", Kind: "inspect", Pkg: "demo/lib", Sym: "Double"}, "ok", "kind", false},
		{"query refs", protocol.Request{Op: "query", Kind: "refs", Pkg: "demo/lib", Sym: "Double"}, "ok", "refs", false},
		{"query callers", protocol.Request{Op: "query", Kind: "callers", Pkg: "demo/lib", Sym: "Double"}, "ok", "callers", false},
		{"query callees", protocol.Request{Op: "query", Kind: "callees", Pkg: "demo", Sym: "main"}, "ok", "callees", false},
		{"query implementations", protocol.Request{Op: "query", Kind: "implementations", Pkg: "demo/lib", Sym: "Store"}, "ok", "direction", false},
		{"query doc", protocol.Request{Op: "query", Kind: "doc", Pkg: "demo/lib", Sym: "Double"}, "ok", "doc", false},
		{"test", protocol.Request{Op: "test", Pkg: "demo/lib"}, "ok", "tests", false},
		{"set-body", protocol.Request{Op: "set-body", Pkg: "demo/lib", Sym: "Double", Body: "return v * 3"}, "accepted", "file", false},
		{"rename", protocol.Request{Op: "rename", Pkg: "demo/lib", Sym: "Tail", To: "Tail2"}, "accepted", "new_name", false},
		{"upsert", protocol.Request{Op: "upsert", Pkg: "demo/lib", Body: "func Quadruple(v int) int { return v * 4 }"}, "accepted", "action", false},
		{"add-param", protocol.Request{Op: "add-param", Pkg: "demo/lib", Sym: "Double", Name: "scale", Type: "int", Def: "1"}, "accepted", "param", false},
		{"patch dry_run", protocol.Request{Op: "patch", Pkg: "demo/lib", Sym: "Double", DryRun: true,
			Ops: json.RawMessage(`[{"op": "rename", "to": "Twice"}]`)}, "ok", "would", false},
		{"unknown", protocol.Request{Op: "frobnicate"}, "error", "error", false},
		{"stop", protocol.Request{Op: "stop"}, "stopping", "", true},
	}
	for _, step := range steps {
		res, stopped := send(step.req)
		if res["status"] != step.status {
			t.Fatalf("%s: status = %v, want %q (response: %v)", step.name, res["status"], step.status, res)
		}
		if step.key != "" {
			if _, ok := res[step.key]; !ok {
				t.Fatalf("%s: response missing %q, wrong handler routed? (response: %v)", step.name, step.key, res)
			}
		}
		if stopped != step.stop {
			t.Fatalf("%s: handle returned stop=%v, want %v", step.name, stopped, step.stop)
		}
	}
	if res, _ := send(protocol.Request{Op: "patch", Pkg: "demo/lib", Sym: "Double", DryRun: true,
		Ops: json.RawMessage(`[{"op": "rename", "to": "Twice"}]`)}); res["would"] != "accepted" {
		t.Fatalf("patch dry_run would = %v, want accepted", res["would"])
	}

	// A request that is not JSON at all still gets a JSON error response,
	// not a dropped connection.
	client, server := net.Pipe()
	done := make(chan bool, 1)
	go func() { done <- handle(server, snap, nil) }()
	if _, err := client.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.NewDecoder(client).Decode(&res); err != nil {
		t.Fatalf("bad request: transport error decoding response: %v", err)
	}
	if <-done; res["status"] != "error" {
		t.Fatalf("bad request: status = %v, want error", res["status"])
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

// AGO_NO_REPAIRS strips the repair channel for the ablation arm: no
// possible_repairs, no resend escalation. did_you_mean and diagnostics
// stay — that is the conventional typed-error baseline being compared
// against.
func TestNoRepairsAblation(t *testing.T) {
	t.Setenv("AGO_NO_REPAIRS", "1")
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
		json.NewEncoder(client).Encode(protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Doub"})
		var res map[string]any
		json.NewDecoder(client).Decode(&res)
		<-done
		return res
	}
	res := send()
	if res["status"] != "rejected" {
		t.Fatalf("want rejected, got %v", res)
	}
	if _, ok := res["possible_repairs"]; ok && res["possible_repairs"] != nil {
		t.Fatalf("ablation must strip possible_repairs: %v", res["possible_repairs"])
	}
	if dym, ok := res["did_you_mean"].([]any); !ok || len(dym) == 0 {
		t.Fatalf("ablation must keep did_you_mean: %v", res["did_you_mean"])
	}
	res = send() // exact resend
	if res["escalation"] != nil || res["resent"] != nil {
		t.Fatalf("ablation must strip escalation: %v", res)
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
