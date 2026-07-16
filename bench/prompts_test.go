package bench

import "testing"

// The raw and semantic workflow prompts must be equivalently sized, or
// prompt budget confounds the mode comparison (proposal §6: parity).
func TestPromptParity(t *testing.T) {
	raw := estimateTokens(promptCommon + promptRaw)
	sem := estimateTokens(promptCommon + promptSemantic)
	lo, hi := raw, sem
	if lo > hi {
		lo, hi = hi, lo
	}
	if float64(lo)/float64(hi) < 0.75 {
		t.Fatalf("prompt sizes diverge: raw≈%d tokens, semantic≈%d tokens", raw, sem)
	}
}
