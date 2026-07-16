package bench

import (
	"testing"
	"time"
)

// The daemon request log's field names and the episode counters that read
// them live in different packages; this round trip fails if either side
// renames a field, instead of counters silently zeroing.
func TestCountersReadRealLogFields(t *testing.T) {
	// One rejected-with-repairs call, one accepted mutation, one exact
	// resend of the rejection — the shapes requestlog.go actually writes.
	start := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	lines := []map[string]any{
		{"ts": start.Add(1 * time.Second).Format(time.RFC3339Nano), "op": "view",
			"outcome": "rejected", "reason": "symbol not found", "repairs": 2.0, "req_sha": "aa", "ms": 3.0},
		{"ts": start.Add(2 * time.Second).Format(time.RFC3339Nano), "op": "view",
			"outcome": "rejected", "reason": "symbol not found", "repairs": 2.0, "req_sha": "aa", "ms": 3.0},
		{"ts": start.Add(3 * time.Second).Format(time.RFC3339Nano), "op": "rename",
			"outcome": "accepted", "req_sha": "bb", "ms": 90.0},
	}
	c := requestCounters(lines, start, start.Add(time.Minute))
	if c["rejects_total"] != 2 || c["repairs_offered"] != 4 || c["resends"] != 1 {
		t.Fatalf("counters misread the log shape: %v", c)
	}
	if c["time_to_first_mutation_s"].(float64) != 3.0 {
		t.Fatalf("time_to_first_mutation_s: %v", c["time_to_first_mutation_s"])
	}
}
