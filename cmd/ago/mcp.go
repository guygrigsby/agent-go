package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"

	"github.com/guygrigsby/agent-go/internal/protocol"
)

// mcpTool is one entry in tools/list's response.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpStr(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func mcpObjSchema(req []string, props map[string]any) map[string]any {
	if req == nil {
		req = []string{} // MCP clients require an array, not null
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

// mcpTools is the exact ten-tool surface docs/specs/language.md "Surface"
// specifies, in that table's order: the six primary tools (status, help,
// query, view, patch, test) then the four sugar ops, each exactly a
// one-op patch (rename, set_body, add_param, upsert_decl). search/inspect/
// refs are not separate tools; their functionality lives under query's
// kind dispatch (kind=search/inspect/refs). tools/list order is asserted
// by TestMCPToolsListExactTenInSpecOrder (mcp_test.go).
func mcpTools() []mcpTool {
	symProps := map[string]any{
		"pkg": mcpStr("package import path"),
		"sym": mcpStr("symbol: Name for package-level, Type.Method for methods and fields"),
	}
	return []mcpTool{
		{"status", "Load or refresh the workspace snapshot. Returns package and file counts and any type errors.",
			mcpObjSchema(nil, map[string]any{})},
		{"help", "Return the versioned op catalog: every patch op's argument schema, one worked example, and its v1 ceilings, plus short descriptions of the six tools. Call this before composing a patch with an unfamiliar op.",
			mcpObjSchema(nil, map[string]any{})},
		{"query", "Semantic questions against the typechecked snapshot: search, inspect, refs, callers, callees, implementations, doc, dispatched by kind. search turns a name fragment from the task into exact pkg/sym addresses; inspect gives kind/signature/declaration position; refs finds every reference including tests. callers/callees are call-graph edges (static calls; a call through an interface resolves to the interface method). implementations works both directions: interface -> implementing types, or concrete type -> interfaces it satisfies. doc returns the plain-text doc comment. List results are position-sorted and paged 50 at a time: count is always the TOTAL found, and a truncated response carries truncated=true plus next_offset — pass that back as offset for the next page.",
			mcpObjSchema([]string{"kind"}, merge(symProps, map[string]any{
				"kind": mcpStr("one of search, inspect, refs, callers, callees, implementations, doc"),
				"q":    mcpStr("name fragment, for kind=search"),
				"offset": map[string]any{"type": "integer",
					"description": "page offset into a list result; pass a prior response's next_offset"}}))},
		{"view", "Render a declaration's source. Functions and methods get a per-statement nK: handle prefix and a generation counter for staleness checks; other declarations (const, var, type) render as plain source. Handles are meaningful only against the generation this same response reports.",
			mcpObjSchema([]string{"pkg", "sym"}, symProps)},
		{"patch", "Apply an ordered list of edit ops as one atomic, generation-checked transaction: every op applies to an in-memory copy, the dirty set re-typechecks once, then everything writes and splices together, or nothing does on any rejection. Ops compose: an op later in the list can address a handle an earlier op returned, referenced as $1, $2, ... by 1-based op index. dry_run runs the identical pipeline and reports accept/reject without writing. Op families, full schemas and examples via help: decl ops (rename, set_body, add_param, upsert_decl, delete_decl, set_doc, add_field, remove_field), statement ops (add_assign, add_if, add_for, add_switch, set_cond, wrap_stmts, wrap_error, ...), test ops (add_test, add_test_case, set_test_case, remove_test_case). A decl or test op (rename, set_body, add_param, upsert_decl, delete_decl, set_doc, add_field, remove_field, add_test, add_test_case, set_test_case, remove_test_case) and a statement op cannot edit the same file in one patch; run them as separate patches.",
			mcpObjSchema([]string{"ops"}, map[string]any{
				"pkg":        mcpStr("default package import path for ops that omit it"),
				"sym":        mcpStr("default symbol for ops that omit it"),
				"generation": map[string]any{"type": "integer", "description": "reject if pkg's generation has moved past this value, from a prior view or patch response"},
				"dry_run":    map[string]any{"type": "boolean", "description": "validate the whole pipeline and report the outcome without writing"},
				"ops": map[string]any{"type": "array", "description": "the ops to apply in order, e.g. [{\"op\":\"add_if\",\"at\":\"n1\",\"where\":\"before\",\"cond\":\"v < 0\"},{\"op\":\"add_return\",\"at\":\"$1\",\"where\":\"first\",\"exprs\":[\"ErrNegative\"]}]; see help for the full catalog",
					"items": map[string]any{"type": "object"}},
			})},
		{"test", "Run `go test`, scoped to a package (default the whole workspace) and optionally filtered by name, and return structured per-test results: pass/fail, elapsed time, and captured output for failures. Validation of mutations stays compiler-only (rename, set_body, patch, ...); this is how you close the behavior loop after a set of changes.",
			mcpObjSchema(nil, map[string]any{
				"pkg": mcpStr("package import path to scope the run to; omit for the whole workspace"),
				"run": mcpStr("test name filter, passed to go test's -run flag")})},
		{"rename", "Rename a symbol at every reference, validated: rejected with compiler diagnostics if the result would not typecheck, collide, or be captured. Nothing is written on rejection. Sugar for a one-op patch; see patch for composing this with other ops in one transaction.",
			mcpObjSchema([]string{"pkg", "sym", "to"}, merge(symProps, map[string]any{"to": mcpStr("new name")}))},
		{"set_body", "Replace a function or method body with Go statements (no surrounding braces), validated: rejected with compiler diagnostics if it would not typecheck. Nothing is written on rejection. Sugar for a one-op patch.",
			mcpObjSchema([]string{"pkg", "sym", "body"}, merge(symProps, map[string]any{"body": mcpStr("new body statements")}))},
		{"add_param", "Append a parameter to a function or method and update every call site to pass the default expression explicitly. Rejected when the function is used as a value or the result would not typecheck. Sugar for a one-op patch.",
			mcpObjSchema([]string{"pkg", "sym", "name", "type"}, merge(symProps, map[string]any{
				"name": mcpStr("new parameter name"), "type": mcpStr("new parameter type, e.g. context.Context"),
				"default": mcpStr("argument expression for existing callers, e.g. context.Background()")}))},
		{"upsert_decl", "Add or replace one whole top-level declaration (func, method, type, const, var) from Go source text. Imports are managed automatically. New declarations go to agent.go; a new package path under the module is created on demand. Rejected with compiler diagnostics if it would not typecheck. Sugar for a one-op patch.",
			mcpObjSchema([]string{"pkg", "text"}, map[string]any{
				"pkg": mcpStr("package import path"), "text": mcpStr("complete declaration source, including doc comment if any")})},
	}
}

// runMCP serves the Model Context Protocol over stdio (newline-delimited
// JSON-RPC). It is a thin stdio wrapper over runMCPIO so tests can drive
// the protocol over an in-memory pipe instead of the process's real stdin/
// stdout.
func runMCP(dir string) error {
	return runMCPIO(dir, os.Stdin, os.Stdout)
}

// runMCPIO forwards each tool call to the workspace daemon, reading
// newline-delimited JSON-RPC requests from r and writing responses to w.
// Rejections are returned as ordinary payloads: they are data for the
// model, not a protocol-level error.
func runMCPIO(dir string, r io.Reader, w io.Writer) error {
	tools := mcpTools()

	in := bufio.NewScanner(r)
	in.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	out := json.NewEncoder(w)
	reply := func(id any, result any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for in.Scan() {
		if len(in.Bytes()) == 0 {
			continue
		}
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				ProtocolVersion string         `json:"protocolVersion"`
				Name            string         `json:"name"`
				Arguments       map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			v := msg.Params.ProtocolVersion
			if v == "" {
				v = "2024-11-05"
			}
			reply(msg.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ago", "version": "0.1"},
			})
		case "tools/list":
			reply(msg.ID, map[string]any{"tools": tools})
		case "tools/call":
			text, isErr := mcpCall(dir, msg.Params.Name, msg.Params.Arguments)
			res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
			if isErr {
				res["isError"] = true
			}
			reply(msg.ID, res)
		default:
			if msg.ID != nil {
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
					"error": map[string]any{"code": -32601, "message": "unknown method " + msg.Method}})
			}
		}
	}
	return in.Err()
}

func mcpCall(dir, name string, args map[string]any) (string, bool) {
	get := func(k string) string { v, _ := args[k].(string); return v }
	if name == "patch" {
		raw, err := json.Marshal(args)
		if err != nil {
			return fmt.Sprintf("bad patch args: %v", err), true
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Sprintf("bad patch args: %v", err), true
		}
		req.Op = "patch"
		out, err := roundTrip(dir, req, true)
		if err != nil {
			return fmt.Sprintf("ago error: %v", err), true
		}
		return out, false
	}
	if name == "query" {
		sym := get("sym")
		if get("kind") == "search" {
			if q := get("q"); q != "" {
				sym = q
			}
		}
		offset, _ := args["offset"].(float64) // JSON numbers decode as float64
		req := protocol.Request{Op: "query", Kind: get("kind"), Pkg: get("pkg"), Sym: sym, Offset: int(offset)}
		out, err := roundTrip(dir, req, true)
		if err != nil {
			return fmt.Sprintf("ago error: %v", err), true
		}
		return out, false
	}
	if name == "test" {
		// run maps to Sym, the same reuse the wire protocol already gives
		// query's q and the daemon's -run filter.
		req := protocol.Request{Op: "test", Pkg: get("pkg"), Sym: get("run")}
		out, err := roundTrip(dir, req, true)
		if err != nil {
			return fmt.Sprintf("ago error: %v", err), true
		}
		return out, false
	}
	// The remaining tools each map straight onto one daemon op: status and
	// help take no addressing args; view and the four sugar ops (rename,
	// set_body, add_param, upsert_decl) do. search/inspect/refs are not
	// tools here — their functionality lives under query's kind dispatch
	// above.
	ops := map[string]string{"status": "status", "help": "help", "view": "view",
		"rename": "rename", "set_body": "set-body", "add_param": "add-param", "upsert_decl": "upsert"}
	op, ok := ops[name]
	if !ok {
		return "unknown tool " + name, true
	}
	body := get("body")
	if op == "upsert" {
		body = get("text")
	}
	req := protocol.Request{Op: op, Pkg: get("pkg"), Sym: get("sym"),
		To: get("to"), Body: body, Name: get("name"), Type: get("type"), Def: get("default")}
	out, err := roundTrip(dir, req, true)
	if err != nil {
		return fmt.Sprintf("ago error: %v", err), true
	}
	return out, false
}

func merge(a, b map[string]any) map[string]any {
	m := map[string]any{}
	maps.Copy(m, a)
	maps.Copy(m, b)
	return m
}
