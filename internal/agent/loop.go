// Package agent is the first-party authoring driver (ADR 0005): a
// one-shot tool loop that drives the ago surface with a local model.
// The model client and the tool executor are both interfaces, so the
// loop's behavior is proven against scripted fakes before any HTTP or
// engine wiring exists.
package agent

import (
	"context"
	"encoding/json"
	"io"
)

// Message is one chat turn in the OpenAI-compatible shape the serving
// side speaks natively; the transcript records these verbatim.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"` // thinking-model chain, transcript-only, never sent back
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a model-requested tool invocation with decoded arguments.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ToolDef describes one callable tool to the model.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// Client is the serving boundary: one completion given the conversation
// so far and the callable tools.
type Client interface {
	Complete(ctx context.Context, msgs []Message, defs []ToolDef) (Message, error)
}

// Tools executes a tool call and reports the result text and whether it
// was an error; the same (text, isErr) shape mcpCall returns.
type Tools interface {
	Call(name string, args map[string]any) (result string, isErr bool)
}

// Config bounds one episode. Wall-clock caps come from the caller's
// context deadline, not a second timer here.
type Config struct {
	MaxSteps   int
	Transcript io.Writer // JSONL, one Message per line; nil to skip
}

// Result is how an episode ended: Stopped is "final", "max_steps", or
// "ctx"; Final is the model's closing text when Stopped is "final".
type Result struct {
	Steps   int
	Stopped string
	Final   string
}

// Run drives one episode: system + task in, completions and tool
// dispatches until the model answers without tool calls or a cap binds.
func Run(ctx context.Context, c Client, tools Tools, defs []ToolDef, system, task string, cfg Config) (*Result, error) {
	res := &Result{}
	record := func(m Message) {
		if cfg.Transcript != nil {
			json.NewEncoder(cfg.Transcript).Encode(m)
		}
	}
	empties := 0
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: task}}
	record(msgs[0])
	record(msgs[1])
	for {
		if ctx.Err() != nil {
			res.Stopped = "ctx"
			return res, nil
		}
		if res.Steps >= cfg.MaxSteps {
			res.Stopped = "max_steps"
			return res, nil
		}
		m, err := c.Complete(ctx, msgs, defs)
		if err != nil {
			// A wall-cap expiry mid completion is a clean stop, not a
			// failure; the transcript up to here is intact.
			if ctx.Err() != nil {
				res.Stopped = "ctx"
				return res, nil
			}
			return res, err
		}
		res.Steps++
		if len(m.ToolCalls) == 0 && m.Content == "" {
			m.ToolCalls = recoverToolCalls(m.Reasoning)
		}
		msgs = append(msgs, m)
		record(m)
		if len(m.ToolCalls) == 0 {
			// Weak local models emit empty messages mid-task; an empty
			// "final" is almost never a completion, so nudge before
			// believing it. Two empties in a row means it has stalled.
			if m.Content == "" {
				empties++
				if empties >= 2 {
					res.Stopped = "empty"
					return res, nil
				}
				nudge := Message{Role: "user", Content: "Continue with the task using the tools. When it is complete, answer with a short summary of what changed."}
				msgs = append(msgs, nudge)
				record(nudge)
				continue
			}
			res.Stopped = "final"
			res.Final = m.Content
			return res, nil
		}
		empties = 0
		for _, tc := range m.ToolCalls {
			out, _ := tools.Call(tc.Name, tc.Args)
			tm := Message{Role: "tool", Content: out, ToolCallID: tc.ID}
			msgs = append(msgs, tm)
			record(tm)
		}
	}
}
