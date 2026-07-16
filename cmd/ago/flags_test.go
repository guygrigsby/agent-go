package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guygrigsby/agent-go/internal/daemon"
	"github.com/guygrigsby/agent-go/internal/protocol"
)

// TestFlagPlumbing walks every daemon verb through the real CLI path:
// newFlagSet defines the flags, fs.Parse consumes the argv a user would
// type, buildRequest maps flags onto the wire request, and roundTrip
// carries it over the same unix socket dial() uses, into a recording stub
// listener standing in for the daemon. The assertion is on the request the
// stub actually received, so a typo anywhere in the plumbing (a flag bound
// to the wrong Request field, a special case dropped) fails here. The
// closing sweep over daemonOps keeps the table complete when a verb is
// added.
func TestFlagPlumbing(t *testing.T) {
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	sock := daemon.SocketPath(abs)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); os.Remove(sock) })

	got := make(chan protocol.Request, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			var req protocol.Request
			if err := json.NewDecoder(conn).Decode(&req); err == nil {
				got <- req
			}
			io.WriteString(conn, `{"status": "ok"}`+"\n")
			conn.Close()
		}
	}()

	bodyFile := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(bodyFile, []byte("return v * 3"), 0o644); err != nil {
		t.Fatal(err)
	}
	patchJSON := `{"pkg":"demo/lib","sym":"Double","generation":7,"dry_run":true,"ops":[{"op":"rename","to":"Twice"}]}`
	patchFile := filepath.Join(dir, "patch.json")
	if err := os.WriteFile(patchFile, []byte(patchJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cmd  string
		args []string
		want protocol.Request
	}{
		{"status", "status", nil, protocol.Request{Op: "status"}},
		{"help", "help", nil, protocol.Request{Op: "help"}},
		{"search", "search", []string{"-s", "Doub"},
			protocol.Request{Op: "search", Sym: "Doub"}},
		{"inspect", "inspect", []string{"-p", "demo/lib", "-s", "Double"},
			protocol.Request{Op: "inspect", Pkg: "demo/lib", Sym: "Double"}},
		{"view", "view", []string{"-p", "demo/lib", "-s", "Store.Put"},
			protocol.Request{Op: "view", Pkg: "demo/lib", Sym: "Store.Put"}},
		{"refs", "refs", []string{"-p", "demo/lib", "-s", "Double"},
			protocol.Request{Op: "refs", Pkg: "demo/lib", Sym: "Double"}},
		{"query search", "query", []string{"--kind", "search", "-q", "Doub"},
			protocol.Request{Op: "query", Kind: "search", Sym: "Doub"}},
		{"query callers", "query", []string{"-k", "callers", "-p", "demo/lib", "-s", "Double"},
			protocol.Request{Op: "query", Kind: "callers", Pkg: "demo/lib", Sym: "Double"}},
		{"set-body", "set-body", []string{"-p", "demo/lib", "-s", "Double", "--body-file", bodyFile},
			protocol.Request{Op: "set-body", Pkg: "demo/lib", Sym: "Double", Body: "return v * 3"}},
		{"upsert", "upsert", []string{"-p", "demo/lib", "--body-file", bodyFile},
			protocol.Request{Op: "upsert", Pkg: "demo/lib", Body: "return v * 3"}},
		{"rename", "rename", []string{"-p", "demo/lib", "-s", "Double", "--to", "Twice"},
			protocol.Request{Op: "rename", Pkg: "demo/lib", Sym: "Double", To: "Twice"}},
		{"add-param", "add-param", []string{"-p", "demo/lib", "-s", "Double", "-n", "scale", "-T", "int", "-d", "1"},
			protocol.Request{Op: "add-param", Pkg: "demo/lib", Sym: "Double", Name: "scale", Type: "int", Default: "1"}},
		{"patch", "patch", []string{"-f", patchFile},
			protocol.Request{Op: "patch", Pkg: "demo/lib", Sym: "Double", Generation: 7, DryRun: true,
				Ops: json.RawMessage(`[{"op":"rename","to":"Twice"}]`)}},
		{"test", "test", []string{"-p", "demo/lib", "--run", "TestDouble"},
			protocol.Request{Op: "test", Pkg: "demo/lib", Sym: "TestDouble"}},
		{"stop", "stop", nil, protocol.Request{Op: "stop"}},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.cmd] = true
		root := newRoot()
		root.SetArgs(append([]string{tc.cmd, "-C", abs}, tc.args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%s: execute: %v", tc.name, err)
		}
		wire := <-got
		gotJSON, _ := json.Marshal(wire)
		wantJSON, _ := json.Marshal(tc.want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s: wire request\n got: %s\nwant: %s", tc.name, gotJSON, wantJSON)
		}
	}
	for _, v := range daemonOps {
		if !covered[v] {
			t.Errorf("daemon verb %q has no flag plumbing case", v)
		}
	}
}

// Root help lists every op, grouped, one line each with its description;
// every op also carries long help and its own flag subset.
func TestUsageCoversEveryOp(t *testing.T) {
	root := newRoot()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	u := buf.String()
	for _, op := range append(append([]string{}, daemonOps...), localOps...) {
		if !strings.Contains(u, "  "+op) {
			t.Errorf("root help missing op %q:\n%s", op, u)
		}
		if opHelp[op] == "" {
			t.Errorf("op %q has no one-line help", op)
		}
		if opLong[op] == "" {
			t.Errorf("op %q has no long help", op)
		}
	}
	for _, title := range []string{"reads and setup:", "mutations", "lifecycle:"} {
		if !strings.Contains(u, title) {
			t.Errorf("root help missing group %q", title)
		}
	}
}
