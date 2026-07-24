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
		{"task": "traefik_1", "mode": "semantic", "profile": "glm", "pass": true, "wall_s": 100.0,
			"resends": 2.0, "repairs_offered": 3.0},
		{"task": "traefik_1", "mode": "semantic", "profile": "glm", "pass": true, "wall_s": 200.0,
			"resends": 1.0, "repairs_offered": 4.0},
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
	if sem.Resends != 3 || sem.RepairsOffered != 7 {
		t.Fatalf("counter sums: resends=%d repairs=%d, want 3 and 7", sem.Resends, sem.RepairsOffered)
	}
	md := renderMarkdown(rows)
	if !strings.Contains(md, "traefik_1") || !strings.Contains(md, "2/3") {
		t.Fatalf("markdown missing cells:\n%s", md)
	}
	if !strings.Contains(md, "| resends | repairs offered |") {
		t.Fatalf("markdown header missing counter columns:\n%s", md)
	}
	for _, line := range strings.Split(strings.TrimSpace(md), "\n") {
		if strings.Contains(line, "semantic") && !strings.Contains(line, "| 3 | 7 |") {
			t.Fatalf("semantic row missing counter cells:\n%s", md)
		}
	}
}

// A benchmark warmup leg left some cells with six iterations where others
// have five. aggregate normalizes every cell to the first five (iter 0-4)
// so k is equal across models; the extra iteration is excluded, not the
// raw evidence, which keeps all six on disk.
func TestAggregateNormalizesToFiveIters(t *testing.T) {
	var episodes []map[string]any
	for i := range 6 {
		episodes = append(episodes, map[string]any{
			"task": "boundary_x", "mode": "raw", "profile": "glm",
			"iter": float64(i), "pass": i < 4, // iters 0-3 pass, 4-5 fail
		})
	}
	rows := aggregate(episodes)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	// iter 5 dropped: N=5 (iters 0-4), passes=4 (iters 0-3).
	if rows[0].N != 5 || rows[0].Passes != 4 {
		t.Fatalf("want N=5 passes=4, got N=%d passes=%d", rows[0].N, rows[0].Passes)
	}
}
