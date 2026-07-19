package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// scriptClient returns its scripted messages in order; a message with tool
// calls makes the loop dispatch them, one without ends the episode.
type scriptClient struct {
	script []Message
	calls  int
	seen   [][]Message
}

func (c *scriptClient) Complete(ctx context.Context, msgs []Message, defs []ToolDef) (Message, error) {
	c.seen = append(c.seen, append([]Message(nil), msgs...))
	m := c.script[c.calls%len(c.script)]
	c.calls++
	return m, nil
}

type recordingTools struct {
	names   []string
	args    []map[string]any
	results []string
}

func (t *recordingTools) Call(name string, args map[string]any) (string, bool) {
	t.names = append(t.names, name)
	t.args = append(t.args, args)
	r := t.results[len(t.names)-1]
	return r, false
}

func TestLoopDispatchesToolCallsThenReturnsFinal(t *testing.T) {
	client := &scriptClient{script: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "query", Args: map[string]any{"kind": "search", "q": "Foo"}}}},
		{Role: "assistant", Content: "renamed Foo to Bar"},
	}}
	tools := &recordingTools{results: []string{`{"count":1}`}}
	var transcript bytes.Buffer
	res, err := Run(context.Background(), client, tools, nil, "you are ago", "rename Foo", Config{MaxSteps: 10, Transcript: &transcript})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "final" || res.Final != "renamed Foo to Bar" || res.Steps != 2 {
		t.Fatalf("res = %+v", res)
	}
	if len(tools.names) != 1 || tools.names[0] != "query" || tools.args[0]["q"] != "Foo" {
		t.Fatalf("tool calls = %v %v", tools.names, tools.args)
	}
	// The second completion must carry the tool result back to the model.
	last := client.seen[1]
	tail := last[len(last)-1]
	if tail.Role != "tool" || tail.Content != `{"count":1}` || tail.ToolCallID != "1" {
		t.Fatalf("tool result message = %+v", tail)
	}
	if first := client.seen[0]; first[0].Role != "system" || first[1].Role != "user" || first[1].Content != "rename Foo" {
		t.Fatalf("opening messages = %+v", first)
	}
}

func TestLoopStopsAtMaxSteps(t *testing.T) {
	client := &scriptClient{script: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "status", Args: map[string]any{}}}},
	}}
	tools := &recordingTools{results: []string{"ok", "ok", "ok"}}
	res, err := Run(context.Background(), client, tools, nil, "sys", "task", Config{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "max_steps" || res.Steps != 3 {
		t.Fatalf("res = %+v", res)
	}
}

func TestLoopStopsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &scriptClient{script: []Message{{Role: "assistant", Content: "unreachable"}}}
	res, err := Run(ctx, client, &recordingTools{}, nil, "sys", "task", Config{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "ctx" || client.calls != 0 {
		t.Fatalf("res = %+v calls = %d", res, client.calls)
	}
}

func TestLoopTranscriptIsJSONL(t *testing.T) {
	client := &scriptClient{script: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "status", Args: map[string]any{}}}},
		{Role: "assistant", Content: "done"},
	}}
	tools := &recordingTools{results: []string{"ok"}}
	var transcript bytes.Buffer
	if _, err := Run(context.Background(), client, tools, nil, "sys", "task", Config{MaxSteps: 10, Transcript: &transcript}); err != nil {
		t.Fatal(err)
	}
	var roles []string
	sc := bufio.NewScanner(&transcript)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad transcript line %q: %v", sc.Text(), err)
		}
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
}

func TestLoopNudgesOnEmptyMessage(t *testing.T) {
	client := &scriptClient{script: []Message{
		{Role: "assistant"}, // empty: no content, no tool calls
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "status", Args: map[string]any{}}}},
		{Role: "assistant", Content: "done"},
	}}
	tools := &recordingTools{results: []string{"ok"}}
	res, err := Run(context.Background(), client, tools, nil, "sys", "task", Config{MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "final" || res.Final != "done" {
		t.Fatalf("res = %+v", res)
	}
	// The empty message must be followed by a user nudge, not an exit.
	second := client.seen[1]
	tail := second[len(second)-1]
	if tail.Role != "user" || tail.Content == "" {
		t.Fatalf("expected nudge after empty message, tail = %+v", tail)
	}
}

func TestLoopStopsAfterConsecutiveEmptyMessages(t *testing.T) {
	client := &scriptClient{script: []Message{{Role: "assistant"}}}
	res, err := Run(context.Background(), client, &recordingTools{}, nil, "sys", "task", Config{MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "empty" {
		t.Fatalf("res = %+v", res)
	}
	if client.calls > 3 {
		t.Fatalf("nudged forever: %d calls", client.calls)
	}
}

type errClient struct{ err error }

func (c *errClient) Complete(ctx context.Context, msgs []Message, defs []ToolDef) (Message, error) {
	return Message{}, c.err
}

func TestLoopDeadlineMidCompletionStopsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &deadlineClient{cancel: cancel}
	res, err := Run(ctx, client, &recordingTools{}, nil, "sys", "task", Config{MaxSteps: 5})
	if err != nil {
		t.Fatalf("wall cap must not surface as an error: %v", err)
	}
	if res.Stopped != "ctx" {
		t.Fatalf("res = %+v", res)
	}
}

// deadlineClient cancels the context during the completion, the shape a
// wall-cap expiry takes mid HTTP request.
type deadlineClient struct{ cancel context.CancelFunc }

func (c *deadlineClient) Complete(ctx context.Context, msgs []Message, defs []ToolDef) (Message, error) {
	c.cancel()
	return Message{}, context.Canceled
}
