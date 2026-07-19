package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Weak local models drift off native tool calling and emit the Qwen
// XML form inside their reasoning channel instead, where the server's
// parser never sees it. Recovering those calls turns a stalled empty
// turn back into progress; the format is exactly what qwen3.5 emitted
// in dogfood transcripts.
var (
	reFunc  = regexp.MustCompile(`(?s)<function=([\w.]+)>(.*?)</function>`)
	reParam = regexp.MustCompile(`(?s)<parameter=([\w.]+)>(.*?)</parameter>`)
)

// recoverToolCalls parses XML-form tool calls out of reasoning text;
// nil when none are present. Parameter values that parse as JSON keep
// their structure (patch ops), everything else stays a string.
func recoverToolCalls(reasoning string) []ToolCall {
	var calls []ToolCall
	for i, fm := range reFunc.FindAllStringSubmatch(reasoning, -1) {
		args := map[string]any{}
		for _, pm := range reParam.FindAllStringSubmatch(fm[2], -1) {
			raw := strings.TrimSpace(pm[2])
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err == nil {
				switch v.(type) {
				case map[string]any, []any, float64, bool:
					args[pm[1]] = v
					continue
				}
			}
			args[pm[1]] = raw
		}
		calls = append(calls, ToolCall{ID: fmt.Sprintf("recovered_%d", i), Name: fm[1], Args: args})
	}
	return calls
}
