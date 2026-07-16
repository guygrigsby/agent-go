package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guygrigsby/agent-go/internal/daemon"
	"github.com/guygrigsby/agent-go/internal/protocol"
)

// TestMCPInitializeAndToolsList drives runMCPIO over an in-memory pipe pair
// (one io.Pipe carrying JSON-RPC requests in, a second carrying responses
// out) instead of the process's real stdin/stdout, so the MCP session is
// testable without a subprocess. It starts a real workspace daemon
// in-process (daemon.Run in a goroutine, bound to the same SocketPath the
// CLI's own dial() computes) against a temp `ago init` fixture, so the path
// exercised is the real one: the same runMCP/mcpCall/roundTrip code an
// actual MCP client drives, hitting a real daemon and a real Help()
// response rather than a stub.
//
// Scope: initialize, tools/list (asserting the spec's exact ten-tool
// surface — docs/specs/language.md "Surface" — in order), and one
// tools/call round trip for "help". A "patch" round trip would need the
// fixture to carry a mutable target symbol and would leave the daemon
// having spliced a file, which complicates cleanup for little extra
// coverage help doesn't already give (mcpCall's patch-request marshaling is
// exercised directly in TestMCPCallPatchWiring below, against the daemon
// but with dry_run so nothing is written); this test's job is the wire
// protocol and tool surface, not another patch-op test.
func TestMCPInitializeAndToolsList(t *testing.T) {
	abs := startMCPFixture(t, "mcptestmod")

	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	mcpDone := make(chan error, 1)
	go func() { mcpDone <- runMCPIO(abs, reqR, respW) }()
	t.Cleanup(func() {
		reqW.Close()
		<-mcpDone
	})

	enc := json.NewEncoder(reqW)
	scanner := bufio.NewScanner(respR)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	nextResult := func(req map[string]any) map[string]any {
		t.Helper()
		if err := enc.Encode(req); err != nil {
			t.Fatalf("encode request: %v", err)
		}
		if !scanner.Scan() {
			t.Fatalf("no response: %v", scanner.Err())
		}
		var resp struct {
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v\nraw: %s", err, scanner.Bytes())
		}
		if resp.Error != nil {
			t.Fatalf("rpc error: %v", resp.Error)
		}
		return resp.Result
	}

	initRes := nextResult(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05"}})
	if initRes["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", initRes["protocolVersion"])
	}

	list := nextResult(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	toolsAny, ok := list["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has no tools array: %v", list)
	}
	// Exactly the spec's ten tools, in the Surface table's order: the six
	// primary tools, then the four sugar ops. search/inspect/refs must NOT
	// appear — their functionality lives under query's kind dispatch.
	want := []string{"status", "help", "query", "view", "patch", "test",
		"rename", "set_body", "add_param", "upsert_decl"}
	if len(toolsAny) != len(want) {
		names := make([]string, len(toolsAny))
		for i, ta := range toolsAny {
			if tm, ok := ta.(map[string]any); ok {
				names[i], _ = tm["name"].(string)
			}
		}
		t.Fatalf("got %d tools, want %d\ngot:  %v\nwant: %v", len(toolsAny), len(want), names, want)
	}
	for i, tAny := range toolsAny {
		tm, ok := tAny.(map[string]any)
		if !ok {
			t.Fatalf("tool %d is not an object: %v", i, tAny)
		}
		if tm["name"] != want[i] {
			t.Errorf("tools[%d] = %v, want %v", i, tm["name"], want[i])
		}
		if desc, _ := tm["description"].(string); desc == "" {
			t.Errorf("tools[%d] (%v) has no description", i, tm["name"])
		}
	}

	call := nextResult(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "help", "arguments": map[string]any{}}})
	if call["isError"] == true {
		t.Fatalf("help call reported isError: %v", call)
	}
	content, ok := call["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call help: no content: %v", call)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("tools/call help: content[0] not an object: %v", content[0])
	}
	text, _ := first["text"].(string)
	var helpRes struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Tools   []any  `json:"tools"`
		Ops     []any  `json:"ops"`
	}
	if err := json.Unmarshal([]byte(text), &helpRes); err != nil {
		t.Fatalf("help response is not JSON: %v\nraw: %s", err, text)
	}
	if helpRes.Status != "ok" {
		t.Errorf("help status = %q", helpRes.Status)
	}
	if helpRes.Version == "" {
		t.Errorf("help version is empty")
	}
	if len(helpRes.Tools) != 6 {
		t.Errorf("help tools = %d entries, want 6", len(helpRes.Tools))
	}
	if len(helpRes.Ops) == 0 {
		t.Errorf("help ops is empty")
	}
}

// TestMCPCallPatchWiring exercises mcpCall's "patch" path specifically: the
// tools/call arguments (a flat map with "ops" as a JSON array) get
// re-marshaled and unmarshaled into protocol.Request, whose Ops field is
// json.RawMessage — a step none of the other tools' wiring goes through.
// dry_run keeps the round trip against the real daemon and the real
// `ago init` fixture's own main() function without writing anything, so
// the test needs no cleanup beyond stopping the daemon.
func TestMCPCallPatchWiring(t *testing.T) {
	abs := startMCPFixture(t, "mcppatchmod")

	text, isErr := mcpCall(abs, "patch", map[string]any{
		"pkg": "mcppatchmod", "sym": "main", "dry_run": true,
		"ops": []any{map[string]any{"op": "rename", "to": "Main"}},
	})
	if isErr {
		t.Fatalf("patch call reported an error: %s", text)
	}
	var res struct {
		Status string `json:"status"`
		Would  string `json:"would"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("patch response is not JSON: %v\nraw: %s", err, text)
	}
	if res.Status != "ok" || res.Would != "accepted" {
		t.Fatalf("dry_run rename of main() was not accepted: %s", text)
	}

	after, err := os.ReadFile(filepath.Join(abs, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "func main()") {
		t.Fatalf("dry_run wrote to disk:\n%s", after)
	}
}

// TestMCPAliasedToolName covers the alias layer: a model that invents an
// "ago_" prefix still lands on the real tool, executes normally, and the
// response carries aliased_from so the model learns the canonical name.
func TestMCPAliasedToolName(t *testing.T) {
	abs := startMCPFixture(t, "mcpaliasmod")

	text, isErr := mcpCall(abs, "ago_view", map[string]any{
		"pkg": "mcpaliasmod", "sym": "main"})
	if isErr {
		t.Fatalf("ago_view reported an error: %s", text)
	}
	var res struct {
		Status      string `json:"status"`
		AliasedFrom string `json:"aliased_from"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("ago_view response is not JSON: %v\nraw: %s", err, text)
	}
	if res.Status != "ok" {
		t.Fatalf("ago_view did not execute as view: %s", text)
	}
	if res.AliasedFrom != "ago_view" {
		t.Errorf("aliased_from = %q, want %q\nraw: %s", res.AliasedFrom, "ago_view", text)
	}
}

// mcpRejectPayload mirrors the redirect payload: the same status/reason/
// did_you_mean/possible_repairs shape internal/snapshot rejects use.
type mcpRejectPayload struct {
	Status          string   `json:"status"`
	Reason          string   `json:"reason"`
	DidYouMean      []string `json:"did_you_mean"`
	PossibleRepairs []struct {
		Why  string `json:"why"`
		Call struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		} `json:"call"`
	} `json:"possible_repairs"`
}

// TestMCPShellToolRedirect covers the hallucinated-shell layer: bash gets a
// structured rejected payload (not an MCP protocol error) whose repairs are
// the two complete calls that reorient the model, query search and help.
func TestMCPShellToolRedirect(t *testing.T) {
	text, isErr := mcpCall(t.TempDir(), "bash", map[string]any{"command": "ls"})
	if isErr {
		t.Fatalf("bash redirect must be data, not an error: %s", text)
	}
	var rej mcpRejectPayload
	if err := json.Unmarshal([]byte(text), &rej); err != nil {
		t.Fatalf("bash redirect is not JSON: %v\nraw: %s", err, text)
	}
	if rej.Status != "rejected" {
		t.Errorf("status = %q, want rejected", rej.Status)
	}
	want := "no shell or file editor here; the workspace is queried and mutated through the ago tools"
	if rej.Reason != want {
		t.Errorf("reason = %q\nwant %q", rej.Reason, want)
	}
	if len(rej.PossibleRepairs) != 2 {
		t.Fatalf("got %d repairs, want 2: %s", len(rej.PossibleRepairs), text)
	}
	first, second := rej.PossibleRepairs[0], rej.PossibleRepairs[1]
	if first.Call.Tool != "query" || first.Call.Args["kind"] != "search" {
		t.Errorf("repair[0] is not the query search call: %s", text)
	}
	if second.Call.Tool != "help" {
		t.Errorf("repair[1] is not the help call: %s", text)
	}
	for i, r := range rej.PossibleRepairs {
		if r.Why == "" {
			t.Errorf("repair[%d] has no why", i)
		}
	}
}

// TestMCPUnknownToolDidYouMean covers the last layer: a near-miss name gets
// the same rejected shape with did_you_mean computed over the real tools.
func TestMCPUnknownToolDidYouMean(t *testing.T) {
	text, isErr := mcpCall(t.TempDir(), "quer", nil)
	if isErr {
		t.Fatalf("unknown-tool redirect must be data, not an error: %s", text)
	}
	var rej mcpRejectPayload
	if err := json.Unmarshal([]byte(text), &rej); err != nil {
		t.Fatalf("redirect is not JSON: %v\nraw: %s", err, text)
	}
	if rej.Status != "rejected" {
		t.Errorf("status = %q, want rejected", rej.Status)
	}
	found := false
	for _, c := range rej.DidYouMean {
		if c == "query" {
			found = true
		}
	}
	if !found {
		t.Errorf("did_you_mean %v does not offer query", rej.DidYouMean)
	}
}

// startMCPFixture builds a temp `ago init` module, starts a real workspace
// daemon against it in-process, and returns the workspace's absolute path.
// The daemon is stopped on test cleanup.
func startMCPFixture(t *testing.T, mod string) string {
	t.Helper()
	dir := t.TempDir()
	if err := runInit(dir, mod); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	daemonDone := make(chan error, 1)
	go func() { daemonDone <- daemon.Run(abs, time.Minute, "") }()
	t.Cleanup(func() {
		roundTrip(abs, protocol.Request{Op: "stop"}, false)
		<-daemonDone
	})
	waitForSocket(t, daemon.SocketPath(abs), 10*time.Second)
	return abs
}

func waitForSocket(t *testing.T, sock string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon socket %s did not appear within %s", sock, timeout)
}
