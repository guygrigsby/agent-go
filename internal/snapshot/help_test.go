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
// JSON it claims to be: an ops-array snippet of one to three ops whose LAST
// element's own "op" field names the entry it belongs to (earlier elements
// set up state the documented op needs — a switch for add_case to extend, a
// scaffolded test for add_test_case to fill). Every element must itself be a
// documented op carrying its required args and no undocumented keys, so the
// setup ops are held to the same schema as the featured one. Whether an
// example actually applies cleanly is TestHelpExamplesAcceptedByFixture's
// job; this one catches structural rot without a loaded workspace.
func TestHelpCatalogExamplesAreValidPatchOps(t *testing.T) {
	byName := map[string]helpOp{}
	for _, op := range opCatalog {
		byName[op.Op] = op
	}
	for _, op := range opCatalog {
		var ops []json.RawMessage
		if err := json.Unmarshal(op.Example, &ops); err != nil {
			t.Errorf("%s: example is not a JSON array: %v", op.Op, err)
			continue
		}
		if len(ops) < 1 || len(ops) > 3 {
			t.Errorf("%s: example should carry one to three ops, got %d", op.Op, len(ops))
			continue
		}
		for i, rawOp := range ops {
			var probe struct {
				Op string `json:"op"`
			}
			if err := json.Unmarshal(rawOp, &probe); err != nil {
				t.Errorf("%s: example op %d does not parse: %v", op.Op, i+1, err)
				continue
			}
			if i == len(ops)-1 && probe.Op != op.Op {
				t.Errorf("%s: example's last op field is %q", op.Op, probe.Op)
			}
			entry, ok := byName[probe.Op]
			if !ok {
				t.Errorf("%s: example op %d names undocumented op %q", op.Op, i+1, probe.Op)
				continue
			}
			// Every required arg must actually be present, and every key must
			// be a documented arg (plus "op") of the op the element names.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rawOp, &raw); err != nil {
				t.Errorf("%s: example op %d is not a JSON object: %v", op.Op, i+1, err)
				continue
			}
			known := map[string]bool{"op": true}
			for _, a := range entry.Args {
				known[a.Name] = true
				if a.Required {
					if _, ok := raw[a.Name]; !ok {
						t.Errorf("%s: example op %d (%s) omits required arg %q", op.Op, i+1, probe.Op, a.Name)
					}
				}
			}
			for k := range raw {
				if !known[k] {
					t.Errorf("%s: example op %d (%s) carries undocumented key %q", op.Op, i+1, probe.Op, k)
				}
			}
		}
	}
}

// TestHelpExamplesAcceptedByFixture lifts every catalog example verbatim into
// a dry_run patch envelope against the demo fixture and requires the full
// pipeline to report it would be accepted. This is the teeth behind the
// examples-as-few-shot contract (docs/specs/cross-model notes): an example
// that parses but rejects against a real workspace teaches every model the
// wrong call. dry_run leaves no writes behind, so one fixture serves all.
func TestHelpExamplesAcceptedByFixture(t *testing.T) {
	s := demo(t)

	// Statement-op examples address handles n1..n3 inside demo/lib.UseHelper;
	// prove the fixture still carries them before blaming an example.
	view, err := s.View("demo/lib", "UseHelper")
	if err != nil {
		t.Fatal(err)
	}
	if n := view["nodes"].(int); n < 3 {
		t.Fatalf("UseHelper has %d handles, examples address n1..n3", n)
	}

	// Envelope targets. Statement ops run inside a fixed function context
	// (UseHelper); decl and test ops need only a package. demo/sig carries
	// the shapes demo/lib lacks: an unreferenced declaration and field, an
	// error-discarding call chain, and a pre-existing table-driven test.
	overrides := map[string]struct{ pkg, sym string }{
		"delete_decl":      {"demo/sig", ""},
		"remove_field":     {"demo/sig", ""},
		"set_test_case":    {"demo/sig", ""},
		"remove_test_case": {"demo/sig", ""},
		"wrap_error":       {"demo/sig", "Run"},
	}

	for _, op := range opCatalog {
		if op.execCeiling != "" {
			t.Logf("%s: example execution skipped: %s", op.Op, op.execCeiling)
			continue
		}
		pkg, sym := "demo/lib", ""
		if !declOps[op.Op] {
			sym = "UseHelper"
		}
		if o, ok := overrides[op.Op]; ok {
			pkg, sym = o.pkg, o.sym
		}
		env := map[string]any{"pkg": pkg, "dry_run": true, "ops": op.Example}
		if sym != "" {
			env["sym"] = sym
		}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Patch(raw)
		if err != nil {
			if rej, ok := err.(*Reject); ok {
				t.Errorf("%s: example rejected: %s (%s) %v", op.Op, rej.Reason, rej.Detail, rej.Diagnostics)
			} else {
				t.Errorf("%s: example failed: %v", op.Op, err)
			}
			continue
		}
		if res["would"] != "accepted" {
			t.Errorf("%s: dry run did not accept: %v", op.Op, res)
		}
	}
}

// TestHelpArgsHaveDescriptions catches a copy-paste gap: every documented
// arg needs a name, a type, and a non-empty description, or the catalog
// stops being useful as a weak model's only reference.
func TestHelpArgsHaveDescriptions(t *testing.T) {
	for _, op := range opCatalog {
		// mod_tidy is the one genuinely argument-free op; anything else
		// with an empty Args list is a copy-paste gap.
		if len(op.Args) == 0 && op.Op != "mod_tidy" {
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
