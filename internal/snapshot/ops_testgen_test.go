package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Task 10: test ops (add_test, add_test_case, set_test_case,
// remove_test_case). demo/lib.Double (func Double(v int) int { return v *
// 2 }) is the target throughout: single param, single comparable result,
// no error — the spec's canonical byte-for-byte skeleton.

func TestAddTestScaffold(t *testing.T) {
	s := demo(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	testFile := filepath.Join(s.dir, "lib", "lib_test.go")
	b, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("lib_test.go not created: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"package lib",
		"func TestDouble(t *testing.T) {",
		"tests := []struct {",
		"v    int",
		"want int",
		"for _, tt := range tests {",
		"t.Run(tt.name, func(t *testing.T) {",
		"if got := Double(tt.v); got != tt.want {",
		`t.Errorf("Double(%v) = %v, want %v", tt.v, got, tt.want)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated test missing %q:\n%s", want, src)
		}
	}
	// Compiles: the op's own post-write typecheck already enforces this, but
	// confirm the symbol is live in the reloaded snapshot too.
	if _, err := s.Inspect("demo/lib", "TestDouble"); err != nil {
		t.Errorf("scaffolded test not queryable: %v", err)
	}
}

func TestAddTestScaffoldNonFunction(t *testing.T) {
	s := demo(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Limit"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol is not a function" {
		t.Fatalf("got %v", err)
	}
}

// A target whose result is a non-comparable type (slice, here) cannot back
// a `got != tt.want` comparison; add_test must reject rather than generate
// code that fails to compile.
func TestAddTestNonComparable(t *testing.T) {
	s := demo(t)
	if _, err := s.UpsertDecl("demo/lib", "func Items() []int { return nil }"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Items"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "result type not comparable; write the test with upsert_decl" {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "lib", "lib_test.go")); err == nil {
		t.Error("rejected add_test still created a test file")
	}
}

// addTestDouble is the shared setup for the add_test_case/set_test_case/
// remove_test_case tests: scaffold TestDouble in its own patch (add_test's
// new-file path reloads the whole workspace; see its own v1-ceiling
// comment), matching the realistic two-step flow — scaffold, then append
// rows in later patches.
func addTestDouble(t *testing.T) *Snapshot {
	t.Helper()
	s := demo(t)
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`)); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAddTestCase appends a row and proves it run-worthy: the generated
// test actually passes when `go test` runs it, not just when it compiles.
func TestAddTestCase(t *testing.T) {
	s := addTestDouble(t)
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib_test.go"))
	src := string(b)
	if !strings.Contains(src, `name: "doubles two"`) || !strings.Contains(src, "v: 2") || !strings.Contains(src, "want: 4") {
		t.Fatalf("row not written:\n%s", src)
	}

	cmd := exec.Command("go", "test", "./lib/", "-run", "TestDouble", "-v")
	cmd.Dir = s.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated test did not pass: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "doubles_two") && !strings.Contains(string(out), "doubles two") {
		t.Errorf("subtest did not run:\n%s", out)
	}
}

// A want value whose type doesn't match its field (a string for an int
// field) is a syntactically valid expression atom — the mismatch only
// surfaces at the pipeline's shared end-of-list typecheck, same discipline
// as every other expression-atom op.
func TestAddTestCaseWrongType(t *testing.T) {
	s := addTestDouble(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"bad","args":["2"],"want":["\"x\""]}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "patch does not typecheck" || len(rej.Diagnostics) == 0 {
		t.Fatalf("want typecheck reject with diagnostics, got %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib_test.go"))
	if strings.Contains(string(b), "bad") {
		t.Errorf("rejected row leaked onto disk:\n%s", b)
	}
}

// An unknown test symbol rejects through the shared findObject not-found
// path (same as any other op addressing a symbol): "symbol not found" and
// (now that suggestSymbols scans test variants) a did_you_mean hit.
func TestAddTestCaseUnknownTest(t *testing.T) {
	s := addTestDouble(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDoubl","name":"x","args":["1"],"want":["2"]}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "symbol not found" {
		t.Fatalf("got %v", err)
	}
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "TestDouble" {
		t.Errorf("want did_you_mean TestDouble, got %v", rej.DidYouMean)
	}

	// Same shape for an unknown CASE (row) name: set_test_case/
	// remove_test_case address an existing row by name and must repair a
	// miss with the table's actual row names.
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"remove_test_case","test":"TestDouble","case":"triples two"}]}`))
	rej, ok = err.(*Reject)
	if !ok || rej.Reason != "test case not found" {
		t.Fatalf("got %v", err)
	}
	if len(rej.DidYouMean) == 0 || rej.DidYouMean[0] != "doubles two" {
		t.Errorf("want did_you_mean doubles two, got %v", rej.DidYouMean)
	}
}

func TestRemoveTestCase(t *testing.T) {
	s := addTestDouble(t)
	for _, row := range []string{
		`{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}`,
		`{"op":"add_test_case","test":"TestDouble","name":"doubles three","args":["3"],"want":["6"]}`,
	} {
		if _, err := s.Patch([]byte(`{"pkg":"demo/lib","ops":[` + row + `]}`)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"remove_test_case","test":"TestDouble","case":"doubles two"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib_test.go"))
	src := string(b)
	if strings.Contains(src, "doubles two") {
		t.Errorf("removed row still present:\n%s", src)
	}
	if !strings.Contains(src, "doubles three") {
		t.Errorf("surviving row removed too:\n%s", src)
	}

	cmd := exec.Command("go", "test", "./lib/", "-run", "TestDouble")
	cmd.Dir = s.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test file broken after removal: %v\n%s", err, out)
	}
}

func TestSetTestCase(t *testing.T) {
	s := addTestDouble(t)
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"set_test_case","test":"TestDouble","case":"doubles two","name":"doubles five","args":["5"],"want":["10"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "accepted" {
		t.Fatalf("got %v", res)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "lib", "lib_test.go"))
	src := string(b)
	if strings.Contains(src, "doubles two") || !strings.Contains(src, "doubles five") || !strings.Contains(src, "want: 10") {
		t.Fatalf("row not replaced:\n%s", src)
	}

	cmd := exec.Command("go", "test", "./lib/", "-run", "TestDouble")
	cmd.Dir = s.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test file broken after replace: %v\n%s", err, out)
	}
}

// add_test's own name defaulting and duplicate rejection.
func TestAddTestDefaultNameAndDuplicate(t *testing.T) {
	s := addTestDouble(t)
	_, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`))
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "test already exists" {
		t.Fatalf("got %v", err)
	}
}
