// Command ago is a semantic query/mutation CLI for Go workspaces. It talks
// to a per-workspace daemon (auto-spawned, idle-exit) that holds the
// typechecked snapshot.
//
//	ago status   [-C dir]
//	ago search   [-C dir] -s <name fragment>
//	ago inspect  [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago view     [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago refs     [-C dir] -p <pkgpath> -s <Name | Recv.Name>
//	ago query    [-C dir] -kind <search|inspect|refs|callers|callees|implementations|doc> [-p pkgpath] [-s Name|Recv.Name] [-q fragment]
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
	"time"

	"github.com/guygrigsby/agent-go/internal/daemon"
	"github.com/guygrigsby/agent-go/internal/protocol"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: ago <init|status|search|inspect|view|refs|query|set-body|upsert|rename|add-param|patch|test|stop|mcp|daemon> [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("C", ".", "workspace directory")
	pkg := fs.String("p", "", "package import path")
	sym := fs.String("s", "", "symbol: Name or Recv.Name")
	bodyFile := fs.String("body-file", "", "new function body (- for stdin)")
	to := fs.String("to", "", "new name for rename")
	pname := fs.String("name", "", "parameter name for add-param")
	ptype := fs.String("type", "", "parameter type for add-param")
	pdefault := fs.String("default", "", "argument expression for existing callers")
	qkind := fs.String("kind", "", "query kind: search|inspect|refs|callers|callees|implementations|doc")
	qfrag := fs.String("q", "", "query fragment (kind=search)")
	run := fs.String("run", "", "test name filter (test op)")
	fs.Parse(args)

	abs, err := filepath.Abs(*dir)
	if err != nil {
		fail("resolve dir: %v", err)
	}

	if cmd == "daemon" {
		if err := daemon.Run(abs, 5*time.Minute); err != nil {
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

	req := protocol.Request{Op: cmd, Pkg: *pkg, Sym: *sym, To: *to, Name: *pname, Type: *ptype, Def: *pdefault}
	if cmd == "set-body" || cmd == "upsert" {
		req.Body = readBody(*bodyFile)
	}
	if cmd == "patch" {
		if err := json.Unmarshal([]byte(readBody(*bodyFile)), &req); err != nil {
			fail("parse patch json: %v", err)
		}
		req.Op = "patch"
	}
	if cmd == "query" {
		req.Kind = *qkind
		if *qfrag != "" {
			req.Sym = *qfrag // wire reuses Sym as q, same as the standalone search op
		}
	}
	if cmd == "test" {
		req.Sym = *run // wire reuses Sym as the -run filter, same as query's q
	}
	if cmd == "status" || cmd == "search" || cmd == "inspect" || cmd == "view" || cmd == "refs" || cmd == "query" || cmd == "set-body" || cmd == "upsert" || cmd == "rename" || cmd == "add-param" || cmd == "patch" || cmd == "test" || cmd == "stop" {
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
