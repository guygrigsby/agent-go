package main

import "testing"

func TestAuthorShapeOf(t *testing.T) {
	shape, ok := authorShapeOf([]string{"libs/echo/echo.go", "libs/echo/echo_test.go"})
	if !ok || shape.Dir != "libs/echo" || len(shape.Impls) != 1 || len(shape.Tests) != 1 {
		t.Fatalf("canonical shape rejected: %+v %v", shape, ok)
	}
	// Two directories is not an author task.
	if _, ok := authorShapeOf([]string{"a/x.go", "b/x_test.go"}); ok {
		t.Fatal("multi-directory commit must not match")
	}
	// Tests without an implementation, or the reverse, is not one either.
	if _, ok := authorShapeOf([]string{"a/x_test.go"}); ok {
		t.Fatal("test-only commit must not match")
	}
	if _, ok := authorShapeOf([]string{"a/x.go"}); ok {
		t.Fatal("untested package must not match: the tests are the spec")
	}
	// Module file edits are the tier 1 ceiling, rejected with the blocker named.
	if _, ok := authorShapeOf([]string{"a/x.go", "a/x_test.go", "go.mod"}); ok {
		t.Fatal("go.mod edits are outside tier 1")
	}
	// Non-Go files in the same new dir (a README) are tolerated and ignored.
	shape, ok = authorShapeOf([]string{"a/x.go", "a/x_test.go", "a/README.md"})
	if !ok || len(shape.Impls) != 1 {
		t.Fatalf("non-Go passengers must not disqualify: %+v %v", shape, ok)
	}
}

func TestParseCoverage(t *testing.T) {
	got, ok := parseCoverage("ok  \texample.com/m/echo\t0.01s\tcoverage: 87.5% of statements\n")
	if !ok || got != 87.5 {
		t.Fatalf("coverage misparsed: %v %v", got, ok)
	}
	if _, ok := parseCoverage("ok example.com/m 0.01s\n"); ok {
		t.Fatal("no coverage figure must not parse")
	}
}
