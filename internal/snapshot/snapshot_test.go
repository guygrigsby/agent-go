package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// demo copies the fixture module to a temp dir; SetBody writes to disk.
func demo(t *testing.T) *Snapshot {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk("testdata/demo", func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("testdata/demo", path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(dst)
}

func TestInspect(t *testing.T) {
	s := demo(t)
	res, err := s.inspect("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "func" || res["type"] != "func(v int) int" {
		t.Errorf("got %v", res)
	}
	res, err = s.inspect("demo/lib", "Store.Put")
	if err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "method" {
		t.Errorf("got %v", res)
	}
}

func TestInspectMissing(t *testing.T) {
	s := demo(t)
	_, err := s.inspect("demo/lib", "Nope")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol not found" {
		t.Fatalf("want symbol-not-found Reject, got %v", err)
	}
}

// Rejections carry repairs: dotted package paths, near-miss symbol names,
// and Recv.Name passed as a rename target all get did_you_mean candidates.
func TestRejectionsSuggestRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.inspect("demo.lib", "Double")
	rej := err.(*Reject)
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "demo/lib" {
		t.Errorf("dotted pkg path: got %v", rej.DidYouMean)
	}
	_, err = s.inspect("demo/lib", "double")
	rej = err.(*Reject)
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "Double" {
		t.Errorf("case miss: got %v", rej.DidYouMean)
	}
	_, err = s.inspect("demo/lib", "Store.put")
	rej = err.(*Reject)
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "Store.Put" {
		t.Errorf("method case miss: got %v", rej.DidYouMean)
	}
	_, err = s.Rename("demo/lib", "Double", "Lib.Twice")
	rej = err.(*Reject)
	if rej.Reason != "new name is not a valid identifier" || len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "Twice" {
		t.Errorf("qualified rename target: got %v %v", rej.Reason, rej.DidYouMean)
	}
}

// Stale names after a rename are subsequences of the new name (or vice
// versa), not substrings. suggestSymbols has a third tier for those.
func TestSuggestSubsequence(t *testing.T) {
	s := demo(t)
	if _, err := s.ensureFresh(); err != nil {
		t.Fatal(err)
	}
	// Typo: "UseHelpr" is not a substring of "UseHelper" either way, but
	// its chars appear in order.
	if got := s.suggestSymbols("demo/lib", "UseHelpr"); len(got) == 0 || got[0] != "UseHelper" {
		t.Errorf("UseHelpr: want [UseHelper ...], got %v", got)
	}
	// Dropped middle: "UHelper" -> "UseHelper". "uhelper" contains "helper"
	// as a substring, so that tier still wins the first slot; the
	// subsequence tier appends UseHelper after it.
	if got := s.suggestSymbols("demo/lib", "UHelper"); len(got) != 2 || got[0] != "helper" || got[1] != "UseHelper" {
		t.Errorf("UHelper: want [helper UseHelper], got %v", got)
	}
	// Vice versa: query longer than candidate, candidate's chars appear in
	// order in the query (rename shrank the symbol).
	if got := s.suggestSymbols("demo/lib", "UseSwarmHelper"); len(got) != 2 || got[0] != "helper" || got[1] != "UseHelper" {
		t.Errorf("UseSwarmHelper: want [helper UseHelper], got %v", got)
	}
}

// Subsequence tier requires len(query) >= 4: "Pt" would subsequence-match
// Store.Put but is too short to mean anything.
func TestSuggestSubsequenceLengthGuard(t *testing.T) {
	s := demo(t)
	if _, err := s.ensureFresh(); err != nil {
		t.Fatal(err)
	}
	if got := s.suggestSymbols("demo/lib", "Pt"); len(got) != 0 {
		t.Errorf("Pt: want no candidates, got %v", got)
	}
}

// Tiers stay ordered: exact before substring before subsequence.
func TestSuggestOrdering(t *testing.T) {
	s := demo(t)
	if _, err := s.ensureFresh(); err != nil {
		t.Fatal(err)
	}
	got := s.suggestSymbols("demo/lib", "UseHelper")
	if len(got) < 2 || got[0] != "UseHelper" || got[1] != "helper" {
		t.Errorf("want exact UseHelper then substring helper, got %v", got)
	}
}

func TestRefs(t *testing.T) {
	s := demo(t)
	res, err := s.refs("demo/lib", "Double", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 2 { // def in lib.go + use in main.go
		t.Errorf("want 2 refs, got %v", res)
	}
}

func TestSearch(t *testing.T) {
	s := demo(t)
	res, err := s.search("dou", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 1 {
		t.Fatalf("got %v", res)
	}
	res, err = s.search("put", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 1 || !strings.Contains(fmt.Sprint(res["symbols"]), "Store.Put") {
		t.Fatalf("got %v", res)
	}
}

func TestSetBodyAccept(t *testing.T) {
	s := demo(t)
	res, err := s.SetBody("demo/lib", "Double", "return v + v")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, err := os.ReadFile(res["file"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "return v + v") {
		t.Errorf("edit not written:\n%s", b)
	}
	// The accept spliced the package in place; queries must see the result
	// with no full reload.
	res, err = s.refs("demo/lib", "Double", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 2 {
		t.Errorf("post-splice refs: got %v", res)
	}
	if res["load_ms"].(int64) != 0 {
		t.Error("accepted mutation triggered a full reload; splice should suffice")
	}
}

// A body edit shifts the positions of every later declaration in the file.
// Cross-package references to those declarations live in packages that were
// not re-typechecked, so identity must survive the drift.
func TestSpliceKeepsIdentityAcrossPositionDrift(t *testing.T) {
	s := demo(t)
	before, err := s.refs("demo/lib", "Tail", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBody("demo/lib", "Double", "w := v\nw += v\nreturn w"); err != nil {
		t.Fatal(err)
	}
	after, err := s.refs("demo/lib", "Tail", 0)
	if err != nil {
		t.Fatal(err)
	}
	if after["count"].(int) != before["count"].(int) {
		t.Errorf("refs of later decl changed after body splice: before %v after %v",
			before["count"], after["count"])
	}
	if after["load_ms"].(int64) != 0 {
		t.Error("query after splice paid a full reload")
	}
}

func TestSetBodyReject(t *testing.T) {
	s := demo(t)
	res, err := s.SetBody("demo/lib", "Double", "return undefinedThing")
	if err == nil {
		t.Fatalf("want rejection, got %v", res)
	}
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "edit does not typecheck" || len(rej.Diagnostics) == 0 {
		t.Fatalf("want typecheck Reject with diagnostics, got %#v", err)
	}
	// Nothing written: original body intact.
	inspect, err := s.inspect("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(strings.SplitN(inspect["pos"].(string), ":", 2)[0])
	if !strings.Contains(string(b), "return v * 2") {
		t.Errorf("rejected edit mutated the file:\n%s", b)
	}
}

func TestSetBodyRejectBadParse(t *testing.T) {
	s := demo(t)
	_, err := s.SetBody("demo/lib", "Double", "return ((")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "new body does not parse" {
		t.Fatalf("want parse Reject, got %v", err)
	}
}

// Rot in an unrelated package must not block a mutation: mined-repo parents
// often carry test files that no longer typecheck against a new toolchain.
func TestMutationAllowedDespiteUnrelatedRot(t *testing.T) {
	s := demo(t)
	rotDir := filepath.Join(s.dir, "rot")
	if err := os.MkdirAll(rotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rot := "package rot\n\nfunc Broken() int { return undefinedThing }\n"
	if err := os.WriteFile(filepath.Join(rotDir, "rot.go"), []byte(rot), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.Rename("demo/lib", "Double", "Twice")
	if err != nil {
		t.Fatalf("rename blocked by unrelated rot: %v", err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	// A mutation whose dirty set includes the rot proceeds too — the
	// baseline contract rejects only NEW diagnostics — and the tolerated
	// rot stays visible via pre_existing.
	res, err = s.UpsertDecl("demo/rot", "func Fine() int {\n\treturn 1\n}")
	if err != nil {
		t.Fatalf("upsert into rotted package blocked: %v", err)
	}
	if res["status"] != "accepted" || res["pre_existing"] == nil {
		t.Fatalf("want accepted with pre_existing set, got %v", res)
	}
}

func TestExternalEditInvalidates(t *testing.T) {
	s := demo(t)
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	// Simulate a human edit behind the daemon's back.
	libGo := filepath.Join(s.dir, "lib", "lib.go")
	b, _ := os.ReadFile(libGo)
	if err := os.WriteFile(libGo, []byte(strings.Replace(string(b), "Double", "Twice", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.inspect("demo/lib", "Twice"); err != nil {
		t.Fatalf("snapshot did not pick up external edit: %v", err)
	}
}

// importFallback must return the primary variant's types: a test-variant
// fork carries its own recompiled *types.Package, and serving it to a
// non-test importer splits the type universe ("cannot use c.scheduler
// (*scheduler.Scheduler) as *scheduler.Scheduler", boundary 687dd1bd).
// The scan order of s.pkgs must not decide identity.
func TestImportFallbackPrefersPrimaryVariant(t *testing.T) {
	s := demo(t)
	if _, err := s.inspect("demo/sig", "Fetch"); err != nil {
		t.Fatal(err)
	}
	// Force the test-variant fork of demo/sig ahead of the primary.
	var reordered []*packages.Package
	for _, p := range s.pkgs {
		if p.PkgPath == "demo/sig" && p.ID != p.PkgPath {
			reordered = append([]*packages.Package{p}, reordered...)
		} else {
			reordered = append(reordered, p)
		}
	}
	s.pkgs = reordered
	got, err := s.importFallback("demo/sig")
	if err != nil {
		t.Fatal(err)
	}
	want := s.primary("demo/sig")
	if got != want.Types {
		t.Fatalf("importFallback returned a non-primary instance: %p vs primary %p", got, want.Types)
	}
}

// A fallback import from inside a test-variant fork must resolve to the
// fork of that path in the same build universe, never the primary: the
// fork's graph siblings carry fork instances, and mixing in the primary
// splits the type universe (687dd1bd: "cannot use c.scheduler
// (*scheduler.Scheduler) as *scheduler.Scheduler" inside [servers.test]).
func TestImportFallbackStaysInForkUniverse(t *testing.T) {
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
	write("go.mod", "module fk\n\ngo 1.24\n")
	write("a/a.go", "package a\n\ntype T int\n")
	write("a/a_int_test.go", "package a\n\nimport \"testing\"\n\nfunc TestInt(t *testing.T) {}\n")
	write("a/a_ext_test.go", "package a_test\n\nimport (\n\t\"testing\"\n\n\t\"fk/a\"\n\t\"fk/b\"\n)\n\nfunc TestT(t *testing.T) {\n\tvar v a.T = b.Give()\n\t_ = v\n}\n")
	write("b/b.go", "package b\n\nimport \"fk/a\"\n\nfunc Give() a.T { return 1 }\n")
	s := New(dir)
	if _, err := s.inspect("fk/b", "Give"); err != nil {
		t.Fatal(err)
	}
	var fork *packages.Package
	for _, p := range s.workspacePackages() {
		if p.PkgPath == "fk/b" && strings.Contains(p.ID, " [") {
			fork = p
		}
	}
	if fork == nil {
		t.Skip("loader produced no fork of fk/b; fixture shape no longer forks")
	}
	got, err := s.importFallbackFor(fork, "fk/a")
	if err != nil {
		t.Fatal(err)
	}
	var wantFork *packages.Package
	for _, p := range s.workspacePackages() {
		if p.PkgPath == "fk/a" && strings.Contains(p.ID, " [") {
			wantFork = p
		}
	}
	if wantFork == nil {
		t.Skip("no fork of fk/a present")
	}
	if got != wantFork.Types {
		t.Fatalf("fallback left the fork universe: got %p, fork %p, primary %p",
			got, wantFork.Types, s.primary("fk/a").Types)
	}
}
