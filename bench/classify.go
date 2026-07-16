package bench

import "strings"

// endpointMarkers are the strings in an agent's combined output that mean
// the serving endpoint, not the harness, failed. Derived from transport
// errors opencode surfaces; extend as real transcripts produce new shapes.
var endpointMarkers = []string{
	"connection refused", "connection reset", "no route to host",
	"i/o timeout", "status 500", "status 502", "status 503", "status 529",
}

// classifyFailure names why an episode did not pass: capped (time),
// endpoint_error (serving stack), harness_crash (agent process died),
// scored_fail (agent finished, result failed scoring). Empty for a pass.
// ponytail: tool_parse detection needs the daemon request log or opencode
// event markers; until then parse failures score as scored_fail.
func classifyFailure(agentErr error, timedOut bool, agentOut string, pass bool) string {
	switch {
	case pass:
		return ""
	case timedOut:
		return "capped"
	case agentErr != nil:
		low := strings.ToLower(agentOut)
		for _, m := range endpointMarkers {
			if strings.Contains(low, m) {
				return "endpoint_error"
			}
		}
		return "harness_crash"
	default:
		return "scored_fail"
	}
}
