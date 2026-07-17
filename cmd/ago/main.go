// Command ago is the CLI front of the semantic edit protocol: every op
// round-trips a protocol.Request to the per-workspace daemon (spawned on
// demand), prints the JSON response, and exits 2 on a rejection.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/guygrigsby/agent-go/internal/daemon"
	"github.com/guygrigsby/agent-go/internal/protocol"
	"github.com/guygrigsby/agent-go/skills"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// daemonOps round-trip a protocol.Request to the workspace daemon;
// localOps run in-process. Together they are the dispatch table main
// builds cobra commands from, and the guard tests key on.
var (
	daemonOps = []string{"status", "help", "search", "inspect", "view", "refs", "query", "set-body", "upsert", "rename", "add-param", "patch", "test"}
	localOps  = []string{"init", "mcp", "daemon", "skill"}
)

// opHelp is each op's one-line description, shown in the grouped root
// help; a unit test keeps it covering the dispatch tables exactly.
var opHelp = map[string]string{
	"init":      "scaffold an agent-first module: MCP wiring, AGENTS.md",
	"status":    "load or refresh the snapshot: packages, files, errors",
	"help":      "the versioned op catalog: args, examples, ceilings",
	"search":    "name fragment to exact symbol addresses",
	"inspect":   "kind, signature, position, doc for one symbol",
	"view":      "declaration as annotated text with node handles",
	"refs":      "every reference, tests included",
	"query":     "semantic questions by --kind (callers, implementations, ...)",
	"set-body":  "replace a function body, typechecked first",
	"upsert":    "add or replace a whole declaration",
	"rename":    "rename a symbol, every reference proven to resolve",
	"add-param": "add a parameter, call sites updated with --default",
	"patch":     "ordered multi-op edit, atomic and generation-checked",
	"test":      "go test, scoped, structured pass/fail",
	"stop":      "stop the workspace daemon",
	"mcp":       "serve the MCP tools over stdio",
	"daemon":    "run the workspace daemon in the foreground",
	"skill":     "install or print the embedded agent skill",
}

// opLong is the per-command help body: what the op does on the wire and
// what comes back.
var opLong = map[string]string{
	"init":      "Creates a compilable module scaffold with MCP wiring and AGENTS.md protocol\ninstructions, ready for an agent to work in. Pass a module path or get one\nderived from the directory name.",
	"status":    "Loads (or refreshes) the workspace snapshot and reports package and file\ncounts plus any pre-existing type errors. The daemon spawns on first use;\nthere is nothing to start.",
	"help":      "Returns the versioned op catalog: every patch op's argument schema, one\nworked example ready to lift verbatim, and any v1 ceilings. The version\nfield changes whenever the catalog shape does.",
	"search":    "Turns a case-insensitive name fragment into exact pkg/sym addresses.\nList responses page at 50: pass a response's next_offset back as --offset.",
	"inspect":   "Reports one symbol's kind, signature, declaration position, and doc\ncomment. For types, the method set is listed with signatures.",
	"view":      "Renders a declaration as annotated text. Functions get one nK: handle per\nstatement plus a generation counter; handles are only meaningful against\nthat generation.",
	"refs":      "Lists every reference to a symbol across the workspace, tests included,\nwith defining occurrences marked.",
	"query":     "Dispatches a semantic question by --kind: search, inspect, refs, callers,\ncallees, implementations, doc. Calls through interfaces report the\ninterface method; implementations bridges to the concrete types.",
	"set-body":  "Replaces a function body with new statements (no surrounding braces).\nThe edit is typechecked against the snapshot before anything is written;\na rejection carries the compiler's diagnostics and exits 2.",
	"upsert":    "Adds or replaces one whole top-level declaration from source text.\nNew declarations land in agent.go, created on demand; a package path that\ndoes not exist yet is created under the module.",
	"rename":    "Renames a symbol and rewrites every reference. Beyond the typecheck, the\nedit proves every rewritten reference still resolves to the renamed\nsymbol, so shadowing capture rejects even when the compiler is satisfied.",
	"add-param": "Adds a parameter and updates every call site with the --default\nexpression. A body local declared as name := default is promoted into\nthe parameter; other collisions reject with their positions.",
	"patch":     "Applies an ordered op list as one atomic, generation-checked transaction:\nall ops validate against an in-memory copy, then everything writes or\nnothing does. The JSON envelope comes from --body-file; see ago help for\nthe op catalog.",
	"test":      "Runs go test scoped to a package (default the whole workspace), optionally\nfiltered by --run, and returns structured per-test results: pass/fail,\nelapsed time, failure output.",
	"stop":      "Stops the workspace daemon. It respawns on the next op.",
	"mcp":       "Serves the ago tools over MCP stdio for agent harnesses. Rejections come\nback as payloads, not tool errors.",
	"daemon":    "Runs the workspace daemon in the foreground (normally auto-spawned).\nIt exits after five minutes idle.",
	"skill":     "The agent skill teaches shell-capable coding agents the ago workflow\nin any Go repository. It ships embedded in this binary, so installing it\nneeds no checkout and no copy step.",
}

// opExample holds per-command examples, shown under Examples: in help.
var opExample = map[string]string{
	"search":    "  ago search -s MaxEntries",
	"inspect":   "  ago inspect -p github.com/acme/store -s Store.Put",
	"view":      "  ago view -p github.com/acme/store -s Put",
	"refs":      "  ago refs -p github.com/acme/store -s MaxEntries",
	"query":     "  ago query -k implementations -p github.com/acme/store -s Putter",
	"set-body":  "  echo 'return v << 1' | ago set-body -p github.com/acme/lib -s Double --body-file -",
	"upsert":    "  ago upsert -p github.com/acme/lib --body-file decl.go",
	"rename":    "  ago rename -p github.com/acme/lib -s MaxEntries --to MaxQuotas",
	"add-param": "  ago add-param -p github.com/acme/lib -s NewLimiter --name ctx --type context.Context --default 'context.Background()'",
	"patch":     "  ago patch --body-file patch.json",
	"test":      "  ago test -p github.com/acme/store --run TestPut",
}

// cliFlags is every flag ago accepts; one value struct shared by the
// cobra commands and the guard tests. registerFlags is the single source
// of truth for names, shorthands, and help strings.
type cliFlags struct {
	dir, pkg, sym, bodyFile, to, name, typ, def, kind, q, run string
	offset                                                    int
}

// opFlags names the flags each op accepts, so per-command help shows only
// what applies. The guard tests iterate this table.
var opFlags = map[string][]string{
	"status":    {},
	"help":      {},
	"search":    {"sym", "q", "offset"},
	"inspect":   {"pkg", "sym"},
	"view":      {"pkg", "sym"},
	"refs":      {"pkg", "sym", "offset"},
	"query":     {"kind", "pkg", "sym", "q", "offset"},
	"set-body":  {"pkg", "sym", "body-file"},
	"upsert":    {"pkg", "body-file"},
	"rename":    {"pkg", "sym", "to"},
	"add-param": {"pkg", "sym", "name", "type", "default"},
	"patch":     {"pkg", "sym", "body-file"},
	"test":      {"pkg", "run"},
	"stop":      {},
}

// registerFlags binds the named flags onto fs. Shorthands: -p pkg, -s sym,
// -q q, -k kind, -f body-file, -r run, -o offset, -n name, -d default,
// -t to, -T type.
func registerFlags(fs *pflag.FlagSet, names []string, f *cliFlags) {
	for _, n := range names {
		switch n {
		case "pkg":
			fs.StringVarP(&f.pkg, "pkg", "p", "", "package import path")
		case "sym":
			fs.StringVarP(&f.sym, "sym", "s", "", "symbol: Name or Recv.Name")
		case "body-file":
			fs.StringVarP(&f.bodyFile, "body-file", "f", "", "input file (- for stdin)")
		case "to":
			fs.StringVarP(&f.to, "to", "t", "", "new name")
		case "name":
			fs.StringVarP(&f.name, "name", "n", "", "parameter name")
		case "type":
			fs.StringVarP(&f.typ, "type", "T", "", "parameter type, e.g. context.Context")
		case "default":
			fs.StringVarP(&f.def, "default", "d", "", "argument expression for existing callers")
		case "kind":
			fs.StringVarP(&f.kind, "kind", "k", "", "query kind: search|inspect|refs|callers|callees|implementations|doc")
		case "q":
			fs.StringVarP(&f.q, "q", "q", "", "query fragment (kind=search)")
		case "run":
			fs.StringVarP(&f.run, "run", "r", "", "test name filter")
		case "offset":
			fs.IntVarP(&f.offset, "offset", "o", 0, "page offset for list results; pass a response's next_offset")
		}
	}
}

// buildRequest maps flag values onto the wire request for a daemon op;
// flags_test.go drives every op through it and asserts the wire fields.
func buildRequest(cmd string, f *cliFlags) (protocol.Request, error) {
	req := protocol.Request{Op: cmd, Pkg: f.pkg, Sym: f.sym, To: f.to,
		Name: f.name, Type: f.typ, Default: f.def, Offset: f.offset}
	switch cmd {
	case "set-body", "upsert":
		req.Body = readBody(f.bodyFile)
	case "patch":
		if err := json.Unmarshal([]byte(readBody(f.bodyFile)), &req); err != nil {
			return req, fmt.Errorf("parse patch json: %w", err)
		}
		req.Op = "patch"
	case "query":
		req.Kind = f.kind
		if f.q != "" {
			req.Sym = f.q // wire reuses Sym as q, same as the standalone search op
		}
	case "test":
		req.Sym = f.run // wire reuses Sym as the -run filter, same as query's q
	}
	return req, nil
}

// newRoot builds the cobra command tree: one subcommand per op, grouped,
// each with its own flag subset, long help, and example.
func newRoot() *cobra.Command {
	f := &cliFlags{}
	root := &cobra.Command{
		Use:           "ago",
		Short:         "semantic edit protocol for Go: query the typechecked workspace, submit compiler-checked mutations",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&f.dir, "dir", "C", ".", "workspace directory")
	root.AddGroup(
		&cobra.Group{ID: "reads", Title: "reads and setup:"},
		&cobra.Group{ID: "mutations", Title: "mutations (validated before anything touches disk):"},
		&cobra.Group{ID: "lifecycle", Title: "lifecycle:"},
	)
	groupOf := map[string]string{
		"init": "reads", "status": "reads", "help": "reads", "search": "reads",
		"inspect": "reads", "view": "reads", "refs": "reads", "query": "reads",
		"set-body": "mutations", "upsert": "mutations", "rename": "mutations",
		"add-param": "mutations", "patch": "mutations", "test": "mutations",
		"mcp": "lifecycle", "daemon": "lifecycle",
	}

	for _, op := range daemonOps {
		op := op
		cmd := &cobra.Command{
			Use:     op,
			Short:   opHelp[op],
			Long:    opLong[op],
			Example: opExample[op],
			GroupID: groupOf[op],
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonOp(op, f)
			},
		}
		registerFlags(cmd.Flags(), opFlags[op], f)
		root.AddCommand(cmd)
	}

	root.AddCommand(&cobra.Command{
		Use: "init [module]", Short: opHelp["init"], Long: opLong["init"],
		GroupID: groupOf["init"], Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(f.dir)
			if err != nil {
				return err
			}
			module := ""
			if len(args) > 0 {
				module = args[0]
			}
			return runInit(abs, module)
		},
	})
	skill := &cobra.Command{
		Use:     "skill",
		Short:   opHelp["skill"],
		Long:    opLong["skill"],
		GroupID: "lifecycle",
	}
	skill.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "install the agent skill under ~/.claude/skills/ago",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dir := filepath.Join(home, ".claude", "skills", "ago")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			dst := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(dst, skills.Ago, 0o644); err != nil {
				return err
			}
			fmt.Printf("installed %s\n", dst)
			return nil
		},
	})
	skill.AddCommand(&cobra.Command{
		Use:   "print",
		Short: "print the embedded skill to stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := cmd.OutOrStdout().Write(skills.Ago)
			return err
		},
	})
	root.AddCommand(skill)

	root.AddCommand(&cobra.Command{
		Use: "mcp", Short: opHelp["mcp"], Long: opLong["mcp"],
		GroupID: groupOf["mcp"], Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(f.dir)
			if err != nil {
				return err
			}
			return runMCP(abs)
		},
	})
	daemonCmd := &cobra.Command{
		Use: "daemon", Short: opHelp["daemon"], Long: opLong["daemon"],
		GroupID: groupOf["daemon"], Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(f.dir)
			if err != nil {
				return err
			}
			return daemon.Run(abs, 5*time.Minute, os.Getenv("AGO_LOG_REQUESTS"))
		},
	}
	daemonCmd.AddCommand(&cobra.Command{
		Use: "stop", Short: opHelp["stop"], Long: opLong["stop"],
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonOp("stop", f)
		},
	})
	root.AddCommand(daemonCmd)
	return root
}

// runDaemonOp round-trips one op and prints the JSON response; a rejected
// mutation exits 2 so scripts can branch on it.
func runDaemonOp(op string, f *cliFlags) error {
	abs, err := filepath.Abs(f.dir)
	if err != nil {
		return err
	}
	req, err := buildRequest(op, f)
	if err != nil {
		return err
	}
	out, err := roundTripIn(abs, req, op != "stop")
	if err != nil {
		return err
	}
	fmt.Print(out)
	var status struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &status) == nil && status.Status == "rejected" {
		os.Exit(2)
	}
	return nil
}

func main() {
	if err := newRoot().Execute(); err != nil {
		fail("%v", err)
	}
}

func roundTripIn(dir string, req protocol.Request, spawn bool) (string, error) {
	return roundTrip(dir, req, spawn)
}

func roundTrip(dir string, req protocol.Request, spawn bool) (string, error) {
	conn, err := dial(dir, spawn)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return "", err
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// dial connects to the workspace daemon, spawning it on demand: the first
// call in a workspace starts `ago daemon` detached and retries until the
// socket answers.
func dial(dir string, spawn bool) (net.Conn, error) {
	sock := daemon.SocketPath(dir)
	conn, err := net.Dial("unix", sock)
	if err == nil {
		return conn, nil
	}
	if !spawn {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "daemon", "-C", dir)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("daemon did not come up on %s", sock)
}

// readBody reads the mutation body from a file, or stdin for "-".
func readBody(path string) string {
	if path == "" {
		return ""
	}
	if path == "-" {
		b, _ := io.ReadAll(os.Stdin)
		return string(b)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read body: %v", err)
	}
	return string(b)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
