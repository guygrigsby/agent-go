package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"

	"github.com/guygrigsby/agent-go/internal/protocol"
)

// runMCP serves the Model Context Protocol over stdio (newline-delimited
// JSON-RPC), forwarding each tool call to the workspace daemon. Rejections
// are returned as ordinary payloads: they are data for the model.
func runMCP(dir string) error {
	type tool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(req []string, props map[string]any) map[string]any {
		if req == nil {
			req = []string{} // MCP clients require an array, not null
		}
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	symProps := map[string]any{
		"pkg": str("package import path"),
		"sym": str("symbol: Name for package-level, Type.Method for methods and fields"),
	}
	tools := []tool{
		{"status", "Load or refresh the workspace snapshot. Returns package and file counts and any type errors.",
			obj(nil, map[string]any{})},
		{"search", "Find workspace symbols by case-insensitive name fragment. Use this first to turn a name from the task into exact pkg and sym addresses.",
			obj([]string{"query"}, map[string]any{"query": str("name fragment to search for")})},
		{"inspect", "Inspect a symbol: kind, type signature, declaration position.",
			obj([]string{"pkg", "sym"}, symProps)},
		{"view", "Render a declaration's source. Functions and methods get a per-statement nK: handle prefix and a generation counter for staleness checks; other declarations (const, var, type) render as plain source.",
			obj([]string{"pkg", "sym"}, symProps)},
		{"refs", "Find every reference to a symbol across the workspace, including tests.",
			obj([]string{"pkg", "sym"}, symProps)},
		{"query", "Semantic questions against the typechecked snapshot: search, inspect, refs, callers, callees, implementations, doc, dispatched by kind. callers/callees are call-graph edges (static calls; a call through an interface resolves to the interface method). implementations works both directions: interface -> implementing types, or concrete type -> interfaces it satisfies. doc returns the plain-text doc comment.",
			obj([]string{"kind"}, merge(symProps, map[string]any{
				"kind": str("one of search, inspect, refs, callers, callees, implementations, doc"),
				"q":    str("name fragment, for kind=search")}))},
		{"rename", "Rename a symbol at every reference, validated: rejected with compiler diagnostics if the result would not typecheck, collide, or be captured. Nothing is written on rejection.",
			obj([]string{"pkg", "sym", "to"}, merge(symProps, map[string]any{"to": str("new name")}))},
		{"upsert_decl", "Add or replace one whole top-level declaration (func, method, type, const, var) from Go source text. Imports are managed automatically. New declarations go to agent.go; a new package path under the module is created on demand. Rejected with compiler diagnostics if it would not typecheck.",
			obj([]string{"pkg", "text"}, map[string]any{
				"pkg": str("package import path"), "text": str("complete declaration source, including doc comment if any")})},
		{"add_param", "Append a parameter to a function or method and update every call site to pass the default expression explicitly. Rejected when the function is used as a value or the result would not typecheck.",
			obj([]string{"pkg", "sym", "name", "type"}, merge(symProps, map[string]any{
				"name": str("new parameter name"), "type": str("new parameter type, e.g. context.Context"),
				"default": str("argument expression for existing callers, e.g. context.Background()")}))},
		{"set_body", "Replace a function or method body with Go statements (no surrounding braces), validated: rejected with compiler diagnostics if it would not typecheck. Nothing is written on rejection.",
			obj([]string{"pkg", "sym", "body"}, merge(symProps, map[string]any{"body": str("new body statements")}))},
		{"patch", "Apply one or more edit operations as a single transaction, generation-checked and rolled back wholesale on any failure. v1 supports exactly one op per patch, drawn from rename, set_body, add_param, upsert_decl; multi-op patches and dry_run arrive in a later release.",
			obj([]string{"ops"}, map[string]any{
				"pkg":        str("default package import path for ops that omit it"),
				"sym":        str("default symbol for ops that omit it"),
				"generation": map[string]any{"type": "integer", "description": "reject if pkg's generation has moved past this value, from a prior view or patch response"},
				"dry_run":    map[string]any{"type": "boolean", "description": "validate without writing; rejected for v1's legacy ops"},
				"ops": map[string]any{"type": "array", "description": "the ops to apply, e.g. [{\"op\":\"rename\",\"to\":\"NewName\"}]",
					"items": map[string]any{"type": "object"}},
			})},
		{"test", "Run `go test`, scoped to a package (default the whole workspace) and optionally filtered by name, and return structured per-test results: pass/fail, elapsed time, and captured output for failures. Validation of mutations stays compiler-only (rename, set_body, patch, ...); this is how you close the behavior loop after a set of changes.",
			obj(nil, map[string]any{
				"pkg": str("package import path to scope the run to; omit for the whole workspace"),
				"run": str("test name filter, passed to go test's -run flag")})},
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	out := json.NewEncoder(os.Stdout)
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
		req := protocol.Request{Op: "query", Kind: get("kind"), Pkg: get("pkg"), Sym: sym}
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
	ops := map[string]string{"status": "status", "search": "search", "inspect": "inspect", "view": "view",
		"refs": "refs", "rename": "rename", "set_body": "set-body", "add_param": "add-param", "upsert_decl": "upsert"}
	op, ok := ops[name]
	if !ok {
		return "unknown tool " + name, true
	}
	sym := get("sym")
	if op == "search" {
		sym = get("query")
	}
	body := get("body")
	if op == "upsert" {
		body = get("text")
	}
	req := protocol.Request{Op: op, Pkg: get("pkg"), Sym: sym,
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
