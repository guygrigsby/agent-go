package bench

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

// mutationOps are the protocol ops that change code; everything else is a
// read. The first accepted one marks time_to_first_mutation_s.
var mutationOps = map[string]bool{
	"rename":    true,
	"set-body":  true,
	"add-param": true,
	"upsert":    true,
	"patch":     true,
}

// requestCounters folds a daemon request log into per-episode counters.
// lines are decoded requests.jsonl records (see internal/daemon/requestlog);
// only records timestamped inside [started, done] count — the rest is
// warm-up or scorer traffic sharing the same log file. Returned keys:
// op_mix (op -> {ok, rejected}), rejects_total, repairs_offered, resends
// (exact re-submissions of an earlier rejected request), and, when an
// accepted mutation exists, time_to_first_mutation_s relative to started.
func requestCounters(lines []map[string]any, started, done time.Time) map[string]any {
	mix := map[string]map[string]int{}
	rejects, repairs, resends := 0, 0, 0
	rejectedSHAs := map[string]bool{}
	var firstMutation float64 = -1
	for _, rec := range lines {
		tsStr, _ := rec["ts"].(string)
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil || ts.Before(started) || ts.After(done) {
			continue
		}
		op, _ := rec["op"].(string)
		outcome, _ := rec["outcome"].(string)
		if mix[op] == nil {
			mix[op] = map[string]int{"ok": 0, "rejected": 0}
		}
		sha, _ := rec["req_sha"].(string)
		if sha != "" && rejectedSHAs[sha] {
			resends++
		}
		if outcome == "rejected" {
			mix[op]["rejected"]++
			rejects++
			repairs += toInt(rec["repairs"])
			if sha != "" {
				rejectedSHAs[sha] = true
			}
			continue
		}
		mix[op]["ok"]++
		if firstMutation < 0 && mutationOps[op] {
			firstMutation = ts.Sub(started).Seconds()
		}
	}
	out := map[string]any{
		"op_mix":          mix,
		"rejects_total":   rejects,
		"repairs_offered": repairs,
		"resends":         resends,
	}
	if firstMutation >= 0 {
		out["time_to_first_mutation_s"] = firstMutation
	}
	return out
}

// toInt reads a count that is float64 after json.Unmarshal but may be an
// int in hand-built records.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// readRequestLog decodes a requests.jsonl file; a missing file or an
// undecodable line yields no records — counters are best-effort evidence,
// never a reason to fail an episode.
func readRequestLog(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			lines = append(lines, rec)
		}
	}
	return lines
}
