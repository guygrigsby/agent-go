package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	res, err := s.Inspect("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "func" || res["type"] != "func(v int) int" {
		t.Errorf("got %v", res)
	}
	res, err = s.Inspect("demo/lib", "Store.Put")
	if err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "method" {
		t.Errorf("got %v", res)
	}
}

func TestInspectMissing(t *testing.T) {
	s := demo(t)
	_, err := s.Inspect("demo/lib", "Nope")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol not found" {
		t.Fatalf("want symbol-not-found Reject, got %v", err)
	}
}

// Rejections carry repairs: dotted package paths, near-miss symbol names,
// and Recv.Name passed as a rename target all get did_you_mean candidates.
func TestRejectionsSuggestRepairs(t *testing.T) {
	s := demo(t)
	_, err := s.Inspect("demo.lib", "Double")
	rej := err.(*Reject)
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "demo/lib" {
		t.Errorf("dotted pkg path: got %v", rej.DidYouMean)
	}
	_, err = s.Inspect("demo/lib", "double")
	rej = err.(*Reject)
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "Double" {
		t.Errorf("case miss: got %v", rej.DidYouMean)
	}
	_, err = s.Inspect("demo/lib", "Store.put")
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

func TestRefs(t *testing.T) {
	s := demo(t)
	res, err := s.Refs("demo/lib", "Double")
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 2 { // def in lib.go + use in main.go
		t.Errorf("want 2 refs, got %v", res)
	}
}

func TestSearch(t *testing.T) {
	s := demo(t)
	res, err := s.Search("dou")
	if err != nil {
		t.Fatal(err)
	}
	if res["count"].(int) != 1 {
		t.Fatalf("got %v", res)
	}
	res, err = s.Search("put")
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
	res, err = s.Refs("demo/lib", "Double")
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
	before, err := s.Refs("demo/lib", "Tail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBody("demo/lib", "Double", "w := v\nw += v\nreturn w"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Refs("demo/lib", "Tail")
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
	inspect, err := s.Inspect("demo/lib", "Double")
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
	// But a mutation whose dirty set includes the rot is still refused.
	_, err = s.UpsertDecl("demo/rot", "func Fine() int {\n\treturn 1\n}")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "affected packages have pre-existing errors" {
		t.Fatalf("want scoped preflight reject, got %v", err)
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
	if _, err := s.Inspect("demo/lib", "Twice"); err != nil {
		t.Fatalf("snapshot did not pick up external edit: %v", err)
	}
}
