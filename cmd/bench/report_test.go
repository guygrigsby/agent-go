package main

import (
	"math"
	"strings"
	"testing"
)

func TestWilson(t *testing.T) {
	lo, hi := wilson(3, 3)
	if math.Abs(lo-0.4385) > 0.01 || hi != 1 {
		t.Fatalf("3/3: got [%f, %f]", lo, hi)
	}
	lo, hi = wilson(0, 3)
	if lo != 0 || math.Abs(hi-0.5615) > 0.01 {
		t.Fatalf("0/3: got [%f, %f]", lo, hi)
	}
	if lo, hi = wilson(0, 0); lo != 0 || hi != 1 {
		t.Fatalf("0/0 must be vacuous: [%f, %f]", lo, hi)
	}
}

func TestAggregateAndRender(t *testing.T) {
	episodes := []map[string]any{
		{"task": "traefik_1", "mode": "semantic", "profile": "glm", "pass": true, "wall_s": 100.0},
		{"task": "traefik_1", "mode": "semantic", "profile": "glm", "pass": true, "wall_s": 200.0},
		{"task": "traefik_1", "mode": "semantic", "profile": "glm", "pass": false, "wall_s": 720.0,
			"capped": true, "failure_kind": "capped"},
		{"task": "traefik_1", "mode": "raw", "profile": "glm", "pass": false, "wall_s": 30.0,
			"failure_kind": "scored_fail"},
	}
	rows := aggregate(episodes)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	sem := rows[0]
	if sem.Mode == "raw" {
		sem = rows[1]
	}
	if sem.N != 3 || sem.Passes != 2 || sem.Capped != 1 {
		t.Fatalf("semantic row wrong: %+v", sem)
	}
	if sem.MedianGreen != 150.0 {
		t.Fatalf("median time-to-green: got %f", sem.MedianGreen)
	}
	if sem.Failures["capped"] != 1 {
		t.Fatalf("failure kinds: %+v", sem.Failures)
	}
	md := renderMarkdown(rows)
	if !strings.Contains(md, "traefik_1") || !strings.Contains(md, "2/3") {
		t.Fatalf("markdown missing cells:\n%s", md)
	}
}
