package bench

import (
	"bufio"
	"encoding/json"
	"strings"
)

// tokenTotals sums opencode's per-step usage from the --format json event
// stream: each step_finish part carries that step's tokens, so a straight
// sum is the episode total. Chosen over the sqlite-by-session route after
// the 2026-07-15 smoke run showed the stream carries everything needed.
type tokenTotals struct {
	In, Out, Reasoning    int
	CacheRead, CacheWrite int
	Steps                 int
}

func sumTokens(transcript string) tokenTotals {
	var tot tokenTotals
	sc := bufio.NewScanner(strings.NewReader(transcript))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Type string `json:"type"`
			Part struct {
				Tokens struct {
					Input     int `json:"input"`
					Output    int `json:"output"`
					Reasoning int `json:"reasoning"`
					Cache     struct {
						Write int `json:"write"`
						Read  int `json:"read"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "step_finish" {
			continue
		}
		tot.In += e.Part.Tokens.Input
		tot.Out += e.Part.Tokens.Output
		tot.Reasoning += e.Part.Tokens.Reasoning
		tot.CacheRead += e.Part.Tokens.Cache.Read
		tot.CacheWrite += e.Part.Tokens.Cache.Write
		tot.Steps++
	}
	return tot
}
