package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func toolCallResponse() string {
	return `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"call_1","type":"function",
		"function":{"name":"query","arguments":"{\"kind\":\"search\",\"q\":\"Foo\"}"}}]}}]}`
}

func TestClientRoundTripsToolCalls(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(toolCallResponse()))
	}))
	defer srv.Close()

	c := NewClient(Options{Endpoint: srv.URL + "/v1", Model: "qwen", APIKey: "sk-test",
		Sampler: map[string]any{"temperature": 0.2}})
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_0", Name: "status", Args: map[string]any{}}}},
		{Role: "tool", Content: "ok", ToolCallID: "call_0"},
	}
	defs := []ToolDef{{Name: "query", Description: "search", Schema: map[string]any{"type": "object"}}}
	got, err := c.Complete(context.Background(), msgs, defs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "query" || got.ToolCalls[0].Args["q"] != "Foo" {
		t.Fatalf("got = %+v", got)
	}
	if body["model"] != "qwen" || body["temperature"] != 0.2 {
		t.Fatalf("body model/sampler = %v %v", body["model"], body["temperature"])
	}
	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "query" {
		t.Fatalf("tools = %v", tools)
	}
	wire := body["messages"].([]any)
	if len(wire) != 4 {
		t.Fatalf("messages = %v", wire)
	}
	// The assistant tool call goes out re-encoded as an arguments string,
	// and the tool result carries its call id.
	asst := wire[2].(map[string]any)
	tc := asst["tool_calls"].([]any)[0].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("assistant tool call = %v", tc)
	}
	toolMsg := wire[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_0" || toolMsg["content"] != "ok" {
		t.Fatalf("tool message = %v", toolMsg)
	}
}

func TestClientRetriesHonoringRetryAfter(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch hits {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(500)
		default:
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
		}
	}))
	defer srv.Close()

	var waits []time.Duration
	c := NewClient(Options{Endpoint: srv.URL + "/v1", Model: "m"})
	c.sleep = func(d time.Duration) { waits = append(waits, d) }
	got, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "done" || hits != 3 {
		t.Fatalf("content=%q hits=%d", got.Content, hits)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] <= 0 {
		t.Fatalf("waits = %v", waits)
	}
}

func TestClientGivesUpAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	c := NewClient(Options{Endpoint: srv.URL + "/v1", Model: "m"})
	c.sleep = func(time.Duration) {}
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, nil); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
}

func TestClientDecodesReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"4",
			"reasoning_content":"thinking about arithmetic"}}]}`))
	}))
	defer srv.Close()
	c := NewClient(Options{Endpoint: srv.URL + "/v1", Model: "m"})
	got, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "2+2"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasoning != "thinking about arithmetic" || got.Content != "4" {
		t.Fatalf("got = %+v", got)
	}
	// Reasoning stays out of the wire format: it is transcript data, not
	// something to feed back to the model.
	var body map[string]any
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv2.Close()
	c2 := NewClient(Options{Endpoint: srv2.URL + "/v1", Model: "m"})
	if _, err := c2.Complete(context.Background(), []Message{got}, nil); err != nil {
		t.Fatal(err)
	}
	if _, present := body["messages"].([]any)[0].(map[string]any)["reasoning_content"]; present {
		t.Fatal("reasoning leaked into the wire")
	}
}
