package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Options configures one serving endpoint: OpenAI-compatible chat
// completions with native tool calling (llama.cpp and the llama-swap
// router both speak it). Sampler entries merge into the request body
// verbatim so profiles carry temperature/top_p/etc without new fields.
type Options struct {
	Endpoint string // base URL up to /v1
	Model    string
	APIKey   string
	Sampler  map[string]any
	Retries  int // attempts after the first; 0 means the default 4
}

// HTTPClient implements Client over /v1/chat/completions. Transient
// failures (429, 5xx) retry with exponential backoff, honoring
// Retry-After in both the seconds and HTTP-date forms.
type HTTPClient struct {
	opt   Options
	http  http.Client
	sleep func(time.Duration)
}

func NewClient(opt Options) *HTTPClient {
	if opt.Retries == 0 {
		opt.Retries = 4
	}
	return &HTTPClient{opt: opt, sleep: time.Sleep}
}

func (c *HTTPClient) Complete(ctx context.Context, msgs []Message, defs []ToolDef) (Message, error) {
	body := map[string]any{"model": c.opt.Model, "messages": wireMessages(msgs)}
	if len(defs) > 0 {
		body["tools"] = wireTools(defs)
		body["tool_choice"] = "auto"
	}
	maps.Copy(body, c.opt.Sampler)
	raw, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}
	var lastStatus string
	var wait time.Duration
	for attempt := 0; attempt <= c.opt.Retries; attempt++ {
		if attempt > 0 {
			c.sleep(wait)
		}
		req, err := http.NewRequestWithContext(ctx, "POST",
			strings.TrimRight(c.opt.Endpoint, "/")+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return Message{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.opt.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.opt.APIKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return Message{}, err
			}
			lastStatus, wait = err.Error(), backoff(attempt+1)
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastStatus = resp.Status
			// Retry-After overrides the exponential step when present.
			if wait = retryAfter(resp); wait == 0 {
				wait = backoff(attempt + 1)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}
		m, err := decodeCompletion(resp)
		resp.Body.Close()
		return m, err
	}
	return Message{}, fmt.Errorf("completion failed after %d attempts: %s", c.opt.Retries+1, lastStatus)
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func wireMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		w := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			w["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				args, _ := json.Marshal(tc.Args)
				tcs = append(tcs, map[string]any{"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": string(args)}})
			}
			w["tool_calls"] = tcs
		}
		out = append(out, w)
	}
	return out
}

func wireTools(defs []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": d.Name, "description": d.Description, "parameters": d.Schema}})
	}
	return out
}

func decodeCompletion(resp *http.Response) (Message, error) {
	var got struct {
		Choices []struct {
			Message struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return Message{}, fmt.Errorf("bad completion response: %w", err)
	}
	if got.Error != nil {
		return Message{}, fmt.Errorf("completion error: %s", got.Error.Message)
	}
	if len(got.Choices) == 0 {
		return Message{}, fmt.Errorf("completion returned no choices")
	}
	cm := got.Choices[0].Message
	m := Message{Role: cm.Role}
	if cm.Content != nil {
		m.Content = *cm.Content
	}
	for _, tc := range cm.ToolCalls {
		args := map[string]any{}
		if tc.Function.Arguments != "" {
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		m.ToolCalls = append(m.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	return m, nil
}
