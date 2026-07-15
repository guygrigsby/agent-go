package snapshot

import (
	"encoding/json"
	"testing"
)

// TestHelpCatalogMatchesOpRegistry keeps help.go honest: every op
// opRegistry's init functions register must be documented in opCatalog, and
// every catalog entry must name a real registered op — no orphans on
// either side, and no duplicate catalog entries.
func TestHelpCatalogMatchesOpRegistry(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range opCatalog {
		if seen[op.Op] {
			t.Errorf("opCatalog: duplicate entry for %q", op.Op)
		}
		seen[op.Op] = true
	}
	for name := range opRegistry {
		if !seen[name] {
			t.Errorf("opRegistry has %q but opCatalog does not document it", name)
		}
	}
	for name := range seen {
		if opRegistry[name] == nil {
			t.Errorf("opCatalog documents %q but opRegistry does not register it", name)
		}
	}
}

// TestHelpCatalogExamplesAreValidPatchOps parses each op's example as the
// JSON it claims to be: a one-element ops-array snippet whose sole
// element's own "op" field names the entry it belongs to. This does not
// prove the example actually applies cleanly (that would require binding
// every example to the demo fixture's exact symbols, coupling generic
// documentation to one test module) — it proves every example is
// well-formed, addressed, wire-shaped patch JSON, catching typos and
// drift from the real arg names the op structs unmarshal.
func TestHelpCatalogExamplesAreValidPatchOps(t *testing.T) {
	for _, op := range opCatalog {
		var ops []json.RawMessage
		if err := json.Unmarshal(op.Example, &ops); err != nil {
			t.Errorf("%s: example is not a JSON array: %v", op.Op, err)
			continue
		}
		if len(ops) != 1 {
			t.Errorf("%s: example should carry exactly one op, got %d", op.Op, len(ops))
			continue
		}
		var probe struct {
			Op string `json:"op"`
		}
		if err := json.Unmarshal(ops[0], &probe); err != nil {
			t.Errorf("%s: example op does not parse: %v", op.Op, err)
			continue
		}
		if probe.Op != op.Op {
			t.Errorf("%s: example's own op field is %q", op.Op, probe.Op)
		}
		// Every required arg must actually be present in the example, and
		// every key in the example must be a documented arg (plus "op").
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(ops[0], &raw); err != nil {
			t.Errorf("%s: example op is not a JSON object: %v", op.Op, err)
			continue
		}
		known := map[string]bool{"op": true}
		for _, a := range op.Args {
			known[a.Name] = true
			if a.Required {
				if _, ok := raw[a.Name]; !ok {
					t.Errorf("%s: example omits required arg %q", op.Op, a.Name)
				}
			}
		}
		for k := range raw {
			if !known[k] {
				t.Errorf("%s: example carries undocumented key %q", op.Op, k)
			}
		}
	}
}

// TestHelpArgsHaveDescriptions catches a copy-paste gap: every documented
// arg needs a name, a type, and a non-empty description, or the catalog
// stops being useful as a weak model's only reference.
func TestHelpArgsHaveDescriptions(t *testing.T) {
	for _, op := range opCatalog {
		if len(op.Args) == 0 {
			t.Errorf("%s: no args documented", op.Op)
		}
		for _, a := range op.Args {
			if a.Name == "" || a.Type == "" || a.Description == "" {
				t.Errorf("%s: incomplete arg doc %+v", op.Op, a)
			}
		}
	}
}

func TestHelpResponseShape(t *testing.T) {
	s := demo(t)
	res, err := s.Help()
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ok" {
		t.Fatalf("status = %v", res["status"])
	}
	if res["version"] != catalogVersion {
		t.Fatalf("version = %v, want %v", res["version"], catalogVersion)
	}
	tools, ok := res["tools"].([]helpTool)
	if !ok || len(tools) != 6 {
		t.Fatalf("tools = %v", res["tools"])
	}
	wantTools := []string{"status", "help", "query", "view", "patch", "test"}
	for i, name := range wantTools {
		if tools[i].Name != name {
			t.Errorf("tools[%d] = %q, want %q", i, tools[i].Name, name)
		}
	}
	ops, ok := res["ops"].([]helpOp)
	if !ok || len(ops) != len(opRegistry) {
		t.Fatalf("ops = %d entries, want %d", len(ops), len(opRegistry))
	}
}

// Help needs neither a lock nor a loaded workspace: it must answer from a
// Snapshot that has never had ensureFresh called on it.
func TestHelpDoesNotRequireLoad(t *testing.T) {
	s := New(t.TempDir())
	res, err := s.Help()
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ok" {
		t.Fatalf("status = %v", res["status"])
	}
}
