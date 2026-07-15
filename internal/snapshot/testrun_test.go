package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task 11: the test tool — scoped `go test` with structured results. Builds
// on Task 10's add_test/add_test_case ops to scaffold a passing and then a
// failing table-driven test against demo/lib, and drives TestRun itself
// end to end. Generated-source shape is Task 10's own coverage; this only
// checks TestRun's parsing and reporting.

func TestTestRun(t *testing.T) {
	s := demo(t)
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`)); err != nil {
		t.Fatal(err)
	}

	res, err := s.TestRun("demo/lib", "")
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ok" {
		t.Fatalf("got %v", res)
	}
	if res["pass"] != true {
		t.Errorf("want pass=true, got %v", res["pass"])
	}
	if res["failed"] != 0 {
		t.Errorf("want failed=0, got %v", res["failed"])
	}
	tests, ok := res["tests"].([]testResult)
	if !ok {
		t.Fatalf("tests is %T, want []testResult", res["tests"])
	}
	found := false
	for _, tr := range tests {
		if strings.Contains(tr.Name, "TestDouble") {
			found = true
			if !tr.Pass {
				t.Errorf("TestDouble reported failing: %+v", tr)
			}
			if tr.Package != "demo/lib" {
				t.Errorf("want package demo/lib, got %q", tr.Package)
			}
		}
	}
	if !found {
		t.Errorf("TestDouble not in results: %+v", tests)
	}

	// Second target, a deliberately wrong want value: typechecks fine
	// (int == int), fails at runtime.
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Tail"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestTail","name":"wrong","args":["2"],"want":["999"]}]}`)); err != nil {
		t.Fatal(err)
	}

	res, err = s.TestRun("demo/lib", "")
	if err != nil {
		t.Fatal(err)
	}
	if res["pass"] != false {
		t.Errorf("want pass=false, got %v", res["pass"])
	}
	if res["failed"] != 1 {
		t.Errorf("want failed=1, got %v", res["failed"])
	}
	tests = res["tests"].([]testResult)
	var sawFailing bool
	for _, tr := range tests {
		if strings.Contains(tr.Name, "TestDouble") && !tr.Pass {
			t.Errorf("TestDouble should still pass: %+v", tr)
		}
		if strings.Contains(tr.Name, "TestTail") {
			if tr.Pass {
				t.Errorf("TestTail should be reported failing: %+v", tr)
				continue
			}
			sawFailing = true
			if tr.Output == "" {
				t.Errorf("failing test has no captured output")
			}
			if strings.Contains(tr.Output, "TestDouble") {
				t.Errorf("failing test's output leaked another test's output: %q", tr.Output)
			}
		}
	}
	if !sawFailing {
		t.Errorf("no failing TestTail entry found: %+v", tests)
	}
}

// A -run filter narrows the run the same way it narrows `go test` itself.
func TestTestRunFilter(t *testing.T) {
	s := demo(t)
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Tail"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestTail","name":"wrong","args":["2"],"want":["999"]}]}`)); err != nil {
		t.Fatal(err)
	}

	res, err := s.TestRun("demo/lib", "TestDouble")
	if err != nil {
		t.Fatal(err)
	}
	if res["pass"] != true {
		t.Errorf("filtered run should exclude the failing test, got %v", res)
	}
	for _, tr := range res["tests"].([]testResult) {
		if strings.Contains(tr.Name, "TestTail") {
			t.Errorf("-run TestDouble should not report TestTail: %+v", tr)
		}
	}
}

// pkg="" scopes the whole workspace (./...), same as the CLI/MCP default.
func TestTestRunWholeWorkspace(t *testing.T) {
	s := demo(t)
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test","target":"Double"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch([]byte(`{"pkg":"demo/lib",
		"ops":[{"op":"add_test_case","test":"TestDouble","name":"doubles two","args":["2"],"want":["4"]}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err := s.TestRun("", "")
	if err != nil {
		t.Fatal(err)
	}
	if res["pass"] != true {
		t.Errorf("got %v", res)
	}
	found := false
	for _, tr := range res["tests"].([]testResult) {
		if strings.Contains(tr.Name, "TestDouble") {
			found = true
		}
	}
	if !found {
		t.Errorf("TestDouble not found scoping whole workspace: %v", res)
	}
}

// A build error (not a test failure) rejects rather than reporting a
// misleadingly clean result. Written directly to disk, bypassing Patch's
// own compiler gate, to simulate rot TestRun must still cope with.
func TestTestRunBuildError(t *testing.T) {
	s := demo(t)
	broken := filepath.Join(s.dir, "lib", "broken_test.go")
	if err := os.WriteFile(broken, []byte("package lib\n\nfunc TestBroken(t *testing.T) { undefinedSymbol() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.TestRun("demo/lib", "")
	rej, ok := err.(*Reject)
	if !ok || rej.Reason != "tests could not run" {
		t.Fatalf("want a 'tests could not run' reject, got %v", err)
	}
	if rej.Detail == "" {
		t.Errorf("want a non-empty detail naming the build error")
	}
	if !strings.Contains(rej.Detail, "undefinedSymbol") {
		t.Errorf("detail should surface the compiler error, got %q", rej.Detail)
	}
}
