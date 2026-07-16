package bench

import (
	"slices"
	"testing"
)

func TestZeroExpr(t *testing.T) {
	cases := map[string]string{
		"context.Context": "context.Background()",
		"*Server":         "nil",
		"[]string":        "nil",
		"map[string]int":  "nil",
		"chan int":        "nil",
		"func()":          "nil",
		"string":          `""`,
		"bool":            "false",
		"int":             "0",
		"int64":           "0",
		"uint32":          "0",
		"float64":         "0",
		"logical.Storage": "nil",
		"time.Duration":   "0",
	}
	for typ, want := range cases {
		if got := zeroExpr(typ); got != want {
			t.Errorf("zeroExpr(%q) = %q, want %q", typ, got, want)
		}
	}
}

func TestModesFor(t *testing.T) {
	if m := modesFor(""); !slices.Equal(m, []string{"raw", "semantic"}) {
		t.Fatalf("default modes: %v", m)
	}
	if m := modesFor("oracle"); !slices.Equal(m, []string{"oracle"}) {
		t.Fatalf("oracle-only: %v", m)
	}
	if m := modesFor("semantic, oracle"); !slices.Equal(m, []string{"semantic", "oracle"}) {
		t.Fatalf("csv: %v", m)
	}
}
