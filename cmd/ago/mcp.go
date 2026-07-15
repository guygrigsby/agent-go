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
		{"refs", "Find every reference to a symbol across the workspace, including tests.",
			obj([]string{"pkg", "sym"}, symProps)},
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
	ops := map[string]string{"status": "status", "search": "search", "inspect": "inspect",
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
