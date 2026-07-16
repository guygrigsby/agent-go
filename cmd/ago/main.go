// Command ago is a semantic query/mutation CLI for Go workspaces. It talks
// to a per-workspace daemon (auto-spawned, idle-exit) that holds the
// typechecked snapshot.
//
//	ago status   [-C dir]
//	ago help     [-C dir]   (versioned op catalog: args, one example, notes per op)
//	ago search   [-C dir] -s <name fragment> [-offset n]
//	ago inspect  [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago view     [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago refs     [-C dir] -p <pkgpath> -s <Name | Recv.Name> [-offset n]
//	ago query    [-C dir] -kind <search|inspect|refs|callers|callees|implementations|doc> [-p pkgpath] [-s Name|Recv.Name] [-q fragment] [-offset n]
//	ago set-body [-C dir] -p <pkgpath> -s <Name | Recv.Name> -body-file <f|->
//	ago upsert   [-C dir] -p <pkgpath> -body-file <f|->   (whole declaration)
//	ago rename   [-C dir] -p <pkgpath> -s <Name | Recv.Name> -to <NewName>
//	ago add-param [-C dir] -p <pkgpath> -s <Name | Recv.Name> -name <n> -type <T> [-default <expr>]
//	ago patch    [-C dir] -body-file <f|->   (full patch envelope: pkg, sym, generation, dry_run, ops)
//	ago test     [-C dir] [-p pkgpath] [-run <filter>]   (go test, scoped, structured results)
//	ago init     [-C dir] [module-path]
//	ago mcp      [-C dir]   (MCP server over stdio, for agent harnesses)
//	ago stop     [-C dir]
//	ago daemon   [-C dir]   (internal; spawned automatically)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/guygrigsby/agent-go/internal/daemon"
	"github.com/guygrigsby/agent-go/internal/protocol"
)

// daemonVerbs round-trip a protocol.Request to the workspace daemon;
// localVerbs run in-process. Together they are the dispatch table main
// routes on, and readme_test.go checks README invocations against them.
var (
	daemonVerbs = []string{"status", "help", "search", "inspect", "view", "refs", "query", "set-body", "upsert", "rename", "add-param", "patch", "test", "stop"}
	localVerbs  = []string{"init", "mcp", "daemon"}
)

// cliFlags is every flag ago accepts — one shared set across verbs.
// newFlagSet is the single source of truth: main parses with it and
// readme_test.go validates README invocations against it.
type cliFlags struct {
	dir, pkg, sym, bodyFile, to, name, typ, def, kind, q, run *string
	offset                                                    *int
}

func newFlagSet(cmd string) (*flag.FlagSet, *cliFlags) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	f := &cliFlags{
		dir:      fs.String("C", ".", "workspace directory"),
		pkg:      fs.String("p", "", "package import path"),
		sym:      fs.String("s", "", "symbol: Name or Recv.Name"),
		bodyFile: fs.String("body-file", "", "new function body (- for stdin)"),
		to:       fs.String("to", "", "new name for rename"),
		name:     fs.String("name", "", "parameter name for add-param"),
		typ:      fs.String("type", "", "parameter type for add-param"),
		def:      fs.String("default", "", "argument expression for existing callers"),
		kind:     fs.String("kind", "", "query kind: search|inspect|refs|callers|callees|implementations|doc"),
		q:        fs.String("q", "", "query fragment (kind=search)"),
		run:      fs.String("run", "", "test name filter (test op)"),
		offset:   fs.Int("offset", 0, "page offset for list results (refs, search, query); pass a response's next_offset"),
	}
	return fs, f
}

// buildRequest maps parsed flags onto the wire request for a daemon verb;
// flags_test.go drives every verb through it and asserts the wire fields.
func buildRequest(cmd string, f *cliFlags) (protocol.Request, error) {
	req := protocol.Request{Op: cmd, Pkg: *f.pkg, Sym: *f.sym, To: *f.to,
		Name: *f.name, Type: *f.typ, Def: *f.def, Offset: *f.offset}
	switch cmd {
	case "set-body", "upsert":
		req.Body = readBody(*f.bodyFile)
	case "patch":
		if err := json.Unmarshal([]byte(readBody(*f.bodyFile)), &req); err != nil {
			return req, fmt.Errorf("parse patch json: %w", err)
		}
		req.Op = "patch"
	case "query":
		req.Kind = *f.kind
		if *f.q != "" {
			req.Sym = *f.q // wire reuses Sym as q, same as the standalone search op
		}
	case "test":
		req.Sym = *f.run // wire reuses Sym as the -run filter, same as query's q
	}
	return req, nil
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: ago <init|%s|mcp|daemon> [flags]", strings.Join(daemonVerbs, "|"))
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs, f := newFlagSet(cmd)
	fs.Parse(args)

	abs, err := filepath.Abs(*f.dir)
	if err != nil {
		fail("resolve dir: %v", err)
	}

	if cmd == "daemon" {
		if err := daemon.Run(abs, 5*time.Minute, os.Getenv("AGO_LOG_REQUESTS")); err != nil {
			fail("daemon: %v", err)
		}
		return
	}
	if cmd == "mcp" {
		if err := runMCP(abs); err != nil {
			fail("mcp: %v", err)
		}
		return
	}
	if cmd == "init" {
		module := ""
		if fs.NArg() > 0 {
			module = fs.Arg(0)
		}
		if err := runInit(abs, module); err != nil {
			fail("init: %v", err)
		}
		return
	}

	req, err := buildRequest(cmd, f)
	if err != nil {
		fail("%v", err)
	}
	if slices.Contains(daemonVerbs, cmd) {
		out, err := roundTrip(abs, req, cmd != "stop")
		if err != nil {
			fail("%v", err)
		}
		fmt.Print(out)
		var status struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(out), &status) == nil && status.Status == "rejected" {
			os.Exit(2)
		}
		return
	}
	fail("unknown command %q", cmd)
}

func roundTrip(dir string, req protocol.Request, spawn bool) (string, error) {
	conn, err := dial(dir, spawn)
	if err != nil {
		if !spawn {
			return `{"status": "ok", "note": "daemon not running"}` + "\n", nil
		}
		return "", err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return "", err
	}
	out, err := io.ReadAll(conn)
	return string(out), err
}

func dial(dir string, spawn bool) (net.Conn, error) {
	sock := daemon.SocketPath(dir)
	conn, err := net.Dial("unix", sock)
	if err == nil || !spawn {
		return conn, err
	}
	os.Remove(sock) // stale socket from a dead daemon
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(sock+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		defer logf.Close()
	}
	cmd := exec.Command(self, "daemon", "-C", dir)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn daemon: %w", err)
	}
	cmd.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not come up on %s (see %s.log)", sock, sock)
}

func readBody(bodyFile string) string {
	if bodyFile == "" {
		fail("set-body requires -body-file (use - for stdin)")
	}
	if bodyFile == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail("read stdin: %v", err)
		}
		return string(b)
	}
	b, err := os.ReadFile(bodyFile)
	if err != nil {
		fail("read body: %v", err)
	}
	return string(b)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
