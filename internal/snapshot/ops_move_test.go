package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// move_decl relocates a self-contained function and requalifies every
// reference: callers in the source package gain the target qualifier,
// callers elsewhere swap qualifiers.
func TestMoveDeclFunc(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	// The symbol now lives in demo/lib.
	if _, err := s.inspect("demo/lib", "Fetch"); err != nil {
		t.Fatalf("Fetch not in target: %v", err)
	}
	if _, err := s.inspect("demo/sig", "Fetch"); err == nil {
		t.Fatal("Fetch still in source")
	}
	// Source-package callers now qualify.
	out, err := s.View("demo/sig", "UseFetch")
	if err != nil {
		t.Fatal(err)
	}
	if text := out["text"].(string); !strings.Contains(text, "lib.Fetch(1, \"x\", 2, 3)") {
		t.Fatalf("source caller not requalified:\n%s", text)
	}
}

// With create_pkg, move_decl creates a missing module-local target package
// and requalifies references into it, one atomic patch (the dp6 ceiling:
// ground-truth commits move decls into packages they create).
func TestMoveDeclCreatePkg(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/util","create_pkg":true}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/util", "Fetch"); err != nil {
		t.Fatalf("Fetch not in created package: %v", err)
	}
	out, err := s.View("demo/sig", "UseFetch")
	if err != nil {
		t.Fatal(err)
	}
	if text := out["text"].(string); !strings.Contains(text, "util.Fetch(") {
		t.Fatalf("source caller not requalified:\n%s", text)
	}
}

// Without create_pkg a missing target still rejects (a typo'd to_pkg must
// never silently create a package), and the rejection offers the same call
// with create_pkg set as a paste-ready repair when the target is
// module-local.
func TestMoveDeclMissingTargetOffersCreateRepair(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/util"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "target package not found" {
		t.Fatalf("got %v", err)
	}
	var repair map[string]any
	for _, r := range rej.PossibleRepairs {
		b, _ := json.Marshal(r.Call)
		if strings.Contains(string(b), `"create_pkg":true`) {
			repair = r.Call
		}
	}
	if repair == nil {
		t.Fatalf("no create_pkg repair offered: %+v", rej.PossibleRepairs)
	}
	// Tenet 3: a repair is executed verbatim, never just displayed.
	args, _ := json.Marshal(repair["args"])
	res, rerr := s.Patch(args)
	if rerr != nil {
		t.Fatalf("offered repair rejected when pasted back: %v", rerr)
	}
	if res["status"] != "accepted" {
		t.Fatalf("offered repair not accepted: %v", res)
	}
}

// A directory that already holds Go files but is not a loaded workspace
// package (a nested module, an excluded dir) must never be silently
// overwritten by create_pkg: vault's sdk/helper/consts is exactly this
// shape, and the first oracle run truncated its agent.go.
func TestMoveDeclCreatePkgRefusesExistingFiles(t *testing.T) {
	s := demo(t)
	dir := filepath.Join(s.dir, "util")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "module demo/util\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	content := "package util\n\nfunc Precious() int { return 42 }\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Fetch","to_pkg":"demo/util","create_pkg":true}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "package exists but did not load" {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "agent.go"))
	if string(after) != content {
		t.Fatalf("existing file clobbered:\n%s", after)
	}
}

// A declaration living in a _test.go file lands in a _test.go file of the
// target package — created on demand when the target has none — never in a
// non-test file where go test would ignore it (boundary dd2c3807 moves
// TestNewDerivedReader alongside NewDerivedReader).
func TestMoveDeclTestFileLandsInTestFile(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"TestSelfContained","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, rerr := os.ReadFile(filepath.Join(s.dir, "lib", "agent_test.go"))
	if rerr != nil || !strings.Contains(string(b), "TestSelfContained") {
		t.Fatalf("test decl not in lib/agent_test.go: %v\n%s", rerr, b)
	}
	for _, f := range []string{"lib.go", "use.go"} {
		if b, _ := os.ReadFile(filepath.Join(s.dir, "lib", f)); strings.Contains(string(b), "TestSelfContained") {
			t.Fatalf("test decl leaked into non-test file %s", f)
		}
	}
}

// The moved declaration's own imports travel with it, aliases included:
// goimports cannot reconstruct an alias (or pick between same-named
// packages — boundary's hkdf collided with a huaweicloud one), so the move
// carries the source file's import spec verbatim.
func TestMoveDeclCarriesImports(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Shout","to_pkg":"demo/util","create_pkg":true}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, rerr := os.ReadFile(filepath.Join(s.dir, "util", "agent.go"))
	if rerr != nil || !strings.Contains(string(b), `str "strings"`) {
		t.Fatalf("aliased import not carried: %v\n%s", rerr, b)
	}
}

// A declaration moving INTO a package it already references drops those
// qualifiers: lib.Double becomes Double once the decl lives in lib.
// Leaving the stale qualifier makes goimports hunt the module cache for
// any package named lib (boundary dd2c3807: crypto.NewDerivedReader
// resolved to hashicorp's unrelated extras/crypto).
func TestMoveDeclStripsTargetQualifiers(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"MovedHome","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	out, err := s.View("demo/lib", "MovedHome")
	if err != nil {
		t.Fatal(err)
	}
	text := out["text"].(string)
	if strings.Contains(text, "lib.Limit") {
		t.Fatalf("stale qualifier survived the move:\n%s", text)
	}
	if !strings.Contains(text, "Limit +") {
		t.Fatalf("reference lost:\n%s", text)
	}
}

// One spec of a grouped const block moves: extracted as a standalone
// declaration in the target, deleted from the group, siblings untouched
// (boundary b26814a3: RecoveryUserId lives in a perms const group).
func TestMoveDeclGroupedSpec(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"ModeFast","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "ModeFast"); err != nil {
		t.Fatalf("moved spec not in target: %v", err)
	}
	if _, err := s.inspect("demo/sig", "ModeSlow"); err != nil {
		t.Fatalf("sibling spec lost: %v", err)
	}
	if _, err := s.inspect("demo/sig", "ModeFast"); err == nil {
		t.Fatal("moved spec still in source group")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(b), `const ModeFast = "fast"`) {
		t.Fatalf("standalone const not materialized:\n%s", b)
	}
}

// A spec that leans on iota cannot stand alone; the reject names it.
func TestMoveDeclGroupedIotaRejects(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"LevelA","to_pkg":"demo/lib"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason+rej.Detail, "iota") {
		t.Fatalf("want iota reject, got %v", err)
	}
}

// A spec that inherits its value from the previous line (LevelB) cannot
// stand alone either.
func TestMoveDeclGroupedInheritedRejects(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"LevelB","to_pkg":"demo/lib"}]}`))
	if _, ok := err.(*Reject); !ok {
		t.Fatalf("want reject, got %v", err)
	}
}

// A type with methods moves as a closure: the type declaration and every
// method travel together (boundary 687dd1bd relocates a job type whose
// whole method set moves with it). Closure-internal references (a method
// calling a sibling method) are fine.
func TestMoveDeclTypeWithMethods(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Counter","to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	if _, err := s.inspect("demo/lib", "Counter"); err != nil {
		t.Fatalf("type not in target: %v", err)
	}
	if _, err := s.inspect("demo/lib", "Counter.Total"); err != nil {
		t.Fatalf("method not in target: %v", err)
	}
	if _, err := s.inspect("demo/sig", "Counter"); err == nil {
		t.Fatal("type still in source")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	for _, want := range []string{"type Counter struct", "func (c *Counter) Add", "func (c *Counter) Total"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("closure incomplete, missing %q:\n%s", want, b)
		}
	}
}

// Several declarations move as one set: syms batches them into a single
// op, intra-set references are legal (the constructor uses the type, the
// test uses both), tests land in a _test.go, and everything validates as
// one atomic patch (boundary 687dd1bd moves a type, its constructor, and
// its tests together).
func TestMoveDeclBatchedSet(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","syms":["Counter","NewCounter","TestCounter"],"to_pkg":"demo/lib"}]}`))
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	for _, sym := range []string{"Counter", "Counter.Total", "NewCounter"} {
		if _, err := s.inspect("demo/lib", sym); err != nil {
			t.Fatalf("%s not in target: %v", sym, err)
		}
	}
	if _, err := s.inspect("demo/sig", "Counter"); err == nil {
		t.Fatal("Counter still in source")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "agent_test.go"))
	if !strings.Contains(string(b), "func TestCounter") {
		t.Fatalf("test mover not in a _test.go:\n%s", b)
	}
	lib, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib.go"))
	if !strings.Contains(string(lib), "func NewCounter") {
		t.Fatalf("constructor not in non-test file:\n%s", lib)
	}
}

// A method leaning on a package-local symbol outside the closure still
// rejects, dependency named.
func TestMoveDeclTypeWithMethodsDepsReject(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"Tainted","to_pkg":"demo/lib"}]}`))
	rej, ok := err.(*Reject)
	if !ok || !strings.Contains(rej.Reason+rej.Detail, "UseFetch") {
		t.Fatalf("want dep reject naming UseFetch, got %v", err)
	}
}

// A declaration that leans on package-local siblings is not self-contained;
// v1 rejects it with the dependency named rather than emitting a broken move.
func TestMoveDeclLocalDepsReject(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/sig","ops":[
		{"op":"move_decl","sym":"UseFetch","to_pkg":"demo/lib"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject, got %v", err)
	}
	if !strings.Contains(rej.Reason+rej.Detail, "Fetch") && rej.Reason != "patch does not typecheck" {
		t.Fatalf("reject must name the local dependency or fail typecheck: %v %v", rej.Reason, rej.Detail)
	}
}

// Moving a declaration whose references pull in a package that itself
// (transitively) imports the target is an import cycle in the real build;
// the engine must reject with the cycle named, never typecheck a
// stale-import world go build rejects. Boundary 687dd1bd is this shape:
// newSessionConnectionCleanupJob(common.ConnectionRepoFactory) moved into
// session while common imports session.
func TestMoveDeclImportCycleRejectsNamed(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module cyc\n\ngo 1.24\n")
	write("target/target.go", "package target\n\ntype T int\n")
	write("shared/shared.go", "package shared\n\nimport \"cyc/target\"\n\ntype F func() target.T\n")
	write("source/source.go", `package source

import "cyc/shared"

func NewJob(f shared.F) int {
	if f == nil {
		return 0
	}
	return 1
}
`)
	s := New(dir)
	_, err := s.Patch([]byte(`{"pkg":"cyc/source","ops":[
		{"op":"move_decl","sym":"NewJob","to_pkg":"cyc/target"}]}`))
	rej, ok := err.(*Reject)
	if !ok {
		t.Fatalf("want Reject naming the import cycle, got %v", err)
	}
	all := rej.Reason + " " + rej.Detail
	for _, d := range rej.Diagnostics {
		all += " " + d.Msg
	}
	if !strings.Contains(all, "cycle") {
		t.Fatalf("reject must name the import cycle, got reason=%q detail=%q diags=%v",
			rej.Reason, rej.Detail, rej.Diagnostics)
	}
}
