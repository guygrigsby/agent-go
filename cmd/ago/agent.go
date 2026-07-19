package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/guygrigsby/agent-go/internal/agent"
)

// agentSystemPrompt teaches the driver's closed surface: the ten ago
// tools for Go, the two file tools for everything else. Evolved from the
// bench's semantic prompt, which the frozen rounds proved on 9B models.
const agentSystemPrompt = `You are a Go authoring agent working through the ago semantic edit protocol. Go source is queried and mutated ONLY through the ago tools; there is no shell and no raw editor for .go files.

Workflow: status to see the workspace, query (kind=search) with a name fragment to find exact symbol addresses, query (kind=refs/callers/inspect) to understand usage, view to read a declaration, then mutate with rename, set_body, add_param, upsert_decl, or a multi-op patch. sym arguments are symbol addresses like Type.Method, never file names. Run test to check behavior. Call help before composing a patch with an unfamiliar op.

Mutations are validated by the compiler before anything lands on disk; a rejection means nothing changed. When a rejection carries possible_repairs, resend the first repair's call exactly as given; never resend a rejected call unchanged.

Non-Go files (go.mod hand-edits, README, configs) use read_file and write_file; write_file rejects .go paths by design.

When the task is complete, answer with a short summary of what changed and stop calling tools.`

// agentToolDefs converts the MCP tool surface plus the two file tools
// into the driver's tool definitions.
func agentToolDefs() []agent.ToolDef {
	var defs []agent.ToolDef
	for _, t := range mcpTools() {
		defs = append(defs, agent.ToolDef{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	defs = append(defs,
		agent.ToolDef{Name: "read_file", Description: "Read any file in the workspace by relative path.",
			Schema: mcpObjSchema([]string{"path"}, map[string]any{"path": mcpStr("relative path")})},
		agent.ToolDef{Name: "write_file", Description: "Write a non-Go file (docs, configs, go.mod). Rejects .go paths: Go source goes through the validated ops.",
			Schema: mcpObjSchema([]string{"path", "content"}, map[string]any{
				"path": mcpStr("relative path"), "content": mcpStr("full file content")})},
	)
	return defs
}

// agentTools multiplexes tool calls: the two file tools run locally, and
// everything else takes the same path as ago mcp, aliases, redirects,
// and repairs included.
type agentTools struct {
	dir   string
	files *agent.FileTools
}

func newAgentTools(dir string) *agentTools {
	return &agentTools{dir: dir, files: agent.NewFileTools(dir)}
}

func (t *agentTools) Call(name string, args map[string]any) (string, bool) {
	if name == "read_file" || name == "write_file" {
		return t.files.Call(name, args)
	}
	return mcpCall(t.dir, name, args)
}

type agentProfile struct {
	Name     string         `json:"name"`
	Endpoint string         `json:"endpoint"`
	Model    string         `json:"model"`
	KeyEnv   string         `json:"key_env"`
	Sampler  map[string]any `json:"sampler"`
}

// loadAgentProfile resolves serving identity: .ago/agent.json supplies
// named profiles, flags override field by field, and flag-only operation
// needs no config at all.
func loadAgentProfile(dir, name, endpoint, model string) (agentProfile, error) {
	var cfg struct {
		Default  string         `json:"default"`
		Profiles []agentProfile `json:"profiles"`
	}
	p := agentProfile{}
	b, err := os.ReadFile(filepath.Join(dir, ".ago", "agent.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return p, fmt.Errorf(".ago/agent.json: %w", err)
		}
		want := name
		if want == "" {
			want = cfg.Default
		}
		found := false
		for _, c := range cfg.Profiles {
			if c.Name == want {
				p, found = c, true
				break
			}
		}
		if !found && want != "" {
			return p, fmt.Errorf("profile %q not in .ago/agent.json", want)
		}
	case name != "":
		return p, fmt.Errorf("profile %q requested but no .ago/agent.json: %w", name, err)
	}
	if endpoint != "" {
		p.Endpoint = endpoint
	}
	if model != "" {
		p.Model = model
	}
	if p.Endpoint == "" || p.Model == "" {
		return p, fmt.Errorf("no serving endpoint: create .ago/agent.json with profiles or pass --endpoint and --model")
	}
	return p, nil
}

// runAgent drives one one-shot episode and prints how it ended.
func runAgent(dir, task, profile, endpoint, model string, maxSteps int, cap time.Duration) error {
	p, err := loadAgentProfile(dir, profile, endpoint, model)
	if err != nil {
		return err
	}
	client := agent.NewClient(agent.Options{
		Endpoint: p.Endpoint, Model: p.Model, APIKey: os.Getenv(p.KeyEnv), Sampler: p.Sampler})

	sessions := filepath.Join(dir, ".ago", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return err
	}
	transcript, err := os.Create(filepath.Join(sessions, time.Now().Format("20060102-150405")+".jsonl"))
	if err != nil {
		return err
	}
	defer transcript.Close()

	ctx, cancel := context.WithTimeout(context.Background(), cap)
	defer cancel()
	res, err := agent.Run(ctx, client, newAgentTools(dir), agentToolDefs(),
		agentSystemPrompt, task, agent.Config{MaxSteps: maxSteps, Transcript: transcript})
	if err != nil {
		return err
	}
	fmt.Printf("stopped: %s after %d steps\n", res.Stopped, res.Steps)
	if res.Final != "" {
		fmt.Println(res.Final)
	}
	// Best-effort change summary; not every workspace is a git repo.
	if out, err := exec.Command("git", "-C", dir, "diff", "--stat").Output(); err == nil && len(out) > 0 {
		fmt.Printf("\n%s", out)
	}
	fmt.Printf("transcript: %s\n", transcript.Name())
	return nil
}
