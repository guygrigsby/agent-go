package agent

import (
	"context"
	"testing"
)

const driftedReasoning = `Let me find the greet package.

<tool_call>
<function=query>
<parameter=kind>
search
</parameter>
<parameter=q>
greet
</parameter>
</function>
</tool_call>`

func TestRecoverToolCallsParsesQwenXMLForm(t *testing.T) {
	calls := recoverToolCalls(driftedReasoning)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Name != "query" || calls[0].Args["kind"] != "search" || calls[0].Args["q"] != "greet" {
		t.Fatalf("call = %+v", calls[0])
	}
	if calls[0].ID == "" {
		t.Fatal("recovered call needs a synthetic id")
	}
}

func TestRecoverToolCallsDecodesJSONParameters(t *testing.T) {
	calls := recoverToolCalls(`<tool_call>
<function=patch>
<parameter=ops>
[{"op":"rename","sym":"Foo","to":"Bar"}]
</parameter>
</function>
</tool_call>`)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	ops, ok := calls[0].Args["ops"].([]any)
	if !ok || len(ops) != 1 {
		t.Fatalf("ops = %#v", calls[0].Args["ops"])
	}
}

func TestRecoverToolCallsIgnoresPlainReasoning(t *testing.T) {
	if calls := recoverToolCalls("just thinking about the problem"); calls != nil {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestLoopRecoversDriftedToolCallsFromReasoning(t *testing.T) {
	client := &scriptClient{script: []Message{
		{Role: "assistant", Reasoning: driftedReasoning},
		{Role: "assistant", Content: "done"},
	}}
	tools := &recordingTools{results: []string{"found it"}}
	res, err := Run(context.Background(), client, tools, nil, "sys", "task", Config{MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "final" || len(tools.names) != 1 || tools.names[0] != "query" {
		t.Fatalf("res = %+v tools = %v", res, tools.names)
	}
	// The recovered result must flow back like a native tool call.
	second := client.seen[1]
	tail := second[len(second)-1]
	if tail.Role != "tool" || tail.Content != "found it" {
		t.Fatalf("tail = %+v", tail)
	}
}
