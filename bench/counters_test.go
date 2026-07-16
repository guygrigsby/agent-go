package bench

import (
	"testing"
	"time"
)

func TestRequestCounters(t *testing.T) {
	started := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	done := started.Add(10 * time.Minute)
	ts := func(d time.Duration) string {
		return started.Add(d).UTC().Format(time.RFC3339Nano)
	}
	lines := []map[string]any{
		// Before the agent window: warm-up traffic, excluded.
		{"ts": ts(-time.Minute), "op": "query", "outcome": "ok", "req_sha": "warm"},
		{"ts": ts(5 * time.Second), "op": "query", "outcome": "ok", "req_sha": "q1"},
		{"ts": ts(10 * time.Second), "op": "view", "outcome": "rejected",
			"reason": "symbol not found", "repairs": float64(3), "req_sha": "v1"},
		// Exact resend of the rejected view.
		{"ts": ts(15 * time.Second), "op": "view", "outcome": "rejected",
			"reason": "symbol not found", "repairs": float64(0), "req_sha": "v1"},
		{"ts": ts(20 * time.Second), "op": "view", "outcome": "ok", "req_sha": "v2"},
		{"ts": ts(30 * time.Second), "op": "rename", "outcome": "rejected",
			"reason": "method or field not found", "repairs": float64(2), "req_sha": "r1"},
		// First accepted mutation, 45s in.
		{"ts": ts(45 * time.Second), "op": "rename", "outcome": "accepted", "req_sha": "r2"},
		// After the agent window: scorer traffic, excluded.
		{"ts": ts(11 * time.Minute), "op": "refs", "outcome": "ok", "req_sha": "score"},
	}
	got := requestCounters(lines, started, done)

	mix, ok := got["op_mix"].(map[string]map[string]int)
	if !ok {
		t.Fatalf("op_mix has wrong type: %T", got["op_mix"])
	}
	want := map[string]map[string]int{
		"query":  {"ok": 1, "rejected": 0},
		"view":   {"ok": 1, "rejected": 2},
		"rename": {"ok": 1, "rejected": 1},
	}
	for op, w := range want {
		if mix[op] == nil || mix[op]["ok"] != w["ok"] || mix[op]["rejected"] != w["rejected"] {
			t.Errorf("op_mix[%s]: got %v, want %v", op, mix[op], w)
		}
	}
	if len(mix) != len(want) {
		t.Errorf("op_mix has %d ops, want %d: %v", len(mix), len(want), mix)
	}
	if got["rejects_total"] != 3 {
		t.Errorf("rejects_total: got %v, want 3", got["rejects_total"])
	}
	if got["repairs_offered"] != 5 {
		t.Errorf("repairs_offered: got %v, want 5", got["repairs_offered"])
	}
	if got["resends"] != 1 {
		t.Errorf("resends: got %v, want 1", got["resends"])
	}
	if ttfm, _ := got["time_to_first_mutation_s"].(float64); ttfm != 45 {
		t.Errorf("time_to_first_mutation_s: got %v, want 45", got["time_to_first_mutation_s"])
	}
}

func TestRequestCountersNoMutation(t *testing.T) {
	started := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	done := started.Add(time.Minute)
	lines := []map[string]any{
		{"ts": started.Add(time.Second).Format(time.RFC3339Nano),
			"op": "query", "outcome": "ok", "req_sha": "q1"},
		// A rejected mutation does not count as the first mutation.
		{"ts": started.Add(2 * time.Second).Format(time.RFC3339Nano),
			"op": "rename", "outcome": "rejected", "repairs": float64(1), "req_sha": "r1"},
	}
	got := requestCounters(lines, started, done)
	if _, present := got["time_to_first_mutation_s"]; present {
		t.Errorf("time_to_first_mutation_s must be absent without an accepted mutation: %v", got)
	}
	if got["rejects_total"] != 1 || got["repairs_offered"] != 1 || got["resends"] != 0 {
		t.Errorf("counters: %v", got)
	}
}

func TestRequestCountersEmpty(t *testing.T) {
	started := time.Now()
	got := requestCounters(nil, started, started.Add(time.Minute))
	if got["rejects_total"] != 0 || got["repairs_offered"] != 0 || got["resends"] != 0 {
		t.Errorf("zero counters expected: %v", got)
	}
	if mix, ok := got["op_mix"].(map[string]map[string]int); !ok || len(mix) != 0 {
		t.Errorf("op_mix must be an empty map: %v", got["op_mix"])
	}
}
