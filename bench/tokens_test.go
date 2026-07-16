package bench

import "testing"

func TestSumTokens(t *testing.T) {
	transcript := `{"type":"step_start","part":{}}
{"type":"step_finish","part":{"tokens":{"total":100,"input":80,"output":20,"reasoning":5,"cache":{"write":10,"read":0}},"cost":0}}
not even json
{"type":"step_finish","part":{"tokens":{"total":50,"input":10,"output":40,"reasoning":0,"cache":{"write":0,"read":70}},"cost":0}}
`
	tot := sumTokens(transcript)
	if tot.In != 90 || tot.Out != 60 || tot.Reasoning != 5 ||
		tot.CacheWrite != 10 || tot.CacheRead != 70 {
		t.Fatalf("got %+v", tot)
	}
	if tot.Steps != 2 {
		t.Fatalf("steps: %d", tot.Steps)
	}
}
