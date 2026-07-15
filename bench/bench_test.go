// Package bench runs the raw-vs-semantic agent comparison as Go benchmarks.
//
// Each benchmark iteration is one full agent episode: fresh worktree at the
// task's parent commit, one opencode run against the configured model, then
// scoring (goal predicate + typecheck + scoped tests). ns/op is wall-clock
// episode time; the "pass" metric is the success rate over iterations.
//
//	AGO_BENCH_ENDPOINT=http://host:port/v1 \
//	AGO_BENCH_MODEL=glm-4.7-flash \
//	AGO_BENCH_SCRATCH=/path/to/clones \
//	go test ./bench -bench Rename -benchtime 3x -timeout 0
//
// Compare modes with benchstat: filter -bench '.../raw' and '.../semantic'
// into separate runs, or slice the emitted results JSONL.
//
// Unset env skips every benchmark, so plain go test ./... stays green.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type RenameSpec struct {
	Pkg string `json:"pkg"`
	Sym string `json:"sym"`
	To  string `json:"to"`
}

type Manifest struct {
	Repo    string       `json:"repo"`
	SHA     string       `json:"sha"`
	Prompt  string       `json:"prompt"`
	Renames []RenameSpec `json:"renames"`
}

type config struct {
	endpoint, model, scratch, agoBin, results string
	cap                                       time.Duration
}

func setup(b *testing.B) config {
	b.Helper()
	c := config{
		endpoint: os.Getenv("AGO_BENCH_ENDPOINT"),
		model:    os.Getenv("AGO_BENCH_MODEL"),
		scratch:  os.Getenv("AGO_BENCH_SCRATCH"),
		results:  os.Getenv("AGO_BENCH_RESULTS"),
		cap:      15 * time.Minute,
	}
	if c.endpoint == "" || c.model == "" || c.scratch == "" {
		b.Skip("set AGO_BENCH_ENDPOINT, AGO_BENCH_MODEL, AGO_BENCH_SCRATCH to run bench")
	}
	if v := os.Getenv("AGO_BENCH_CAP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			b.Fatalf("AGO_BENCH_CAP: %v", err)
		}
		c.cap = d
	}
	if c.results == "" {
		c.results = "results.jsonl"
	}
	bin := filepath.Join(b.TempDir(), "ago")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/guygrigsby/agent-go/cmd/ago").CombinedOutput()
	if err != nil {
		b.Fatalf("build ago: %v\n%s", err, out)
	}
	c.agoBin = bin
	return c
}

func BenchmarkRename(b *testing.B) {
	c := setup(b)
	raw, err := os.ReadFile("tasks-rename.json")
	if err != nil {
		b.Fatal(err)
	}
	var tasks []Manifest
	if err := json.Unmarshal(raw, &tasks); err != nil {
		b.Fatal(err)
	}
	for _, t := range tasks {
		if len(t.Renames) == 0 {
			continue
		}
		for _, mode := range []string{"raw", "semantic"} {
			b.Run(fmt.Sprintf("%s_%s/%s", t.Repo, t.SHA[:8], mode), func(b *testing.B) {
				passes := 0
				for range b.N {
					if episode(b, c, t, mode) {
						passes++
					}
				}
				b.ReportMetric(float64(passes)/float64(b.N), "pass")
			})
		}
	}
}

// episode runs one agent attempt and scores it. Only the agent run is on
// the clock; worktree setup, cache warming, and scoring are not.
func episode(b *testing.B, c config, t Manifest, mode string) bool {
	b.Helper()
	b.StopTimer()
	wt := worktree(b, c, t)
	defer teardown(c, t, wt)
	writeOpencodeConfig(b, c, wt, mode)
	// Warm what any agent would find warm on a dev machine: module cache,
	// build cache, and (semantic mode) the daemon snapshot.
	run(wt, c.cap, "go", "mod", "download")
	run(wt, c.cap, "go", "build", "./...")
	baseline := map[string]int{}
	for _, r := range t.Renames {
		baseline[r.Pkg+"."+r.Sym] = refCount(c, wt, r.Pkg, r.Sym)
	}
	if mode == "raw" {
		agoStop(c, wt) // raw mode gets no daemon; scoring respawns it later
	}

	b.StartTimer()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.cap)
	agentOut, agentErr := runAgent(ctx, c, wt, t.Prompt)
	cancel()
	wall := time.Since(start)
	b.StopTimer()

	res := score(c, wt, t, baseline)
	res["task"] = t.Repo + "_" + t.SHA[:8]
	res["mode"] = mode
	res["wall_s"] = wall.Seconds()
	res["capped"] = ctx.Err() != nil
	res["agent_error"] = agentErr != nil
	appendResult(c.results, res)
	pass, _ := res["pass"].(bool)
	if !pass && testing.Verbose() {
		b.Logf("FAIL %s/%s: %v\nagent tail: %s", res["task"], mode, res, tail(agentOut, 800))
	}
	return pass
}

func score(c config, wt string, t Manifest, baseline map[string]int) map[string]any {
	predicate := true
	for _, r := range t.Renames {
		oldGone := refCount(c, wt, r.Pkg, r.Sym) == 0
		newRefs := refCount(c, wt, r.Pkg, renamedSym(r))
		if !oldGone || newRefs != baseline[r.Pkg+"."+r.Sym] {
			predicate = false
			break
		}
	}
	typecheck := false
	if out := agoJSON(c, wt, "status"); out != nil {
		errs, _ := out["errors"].([]any)
		typecheck = out["status"] == "ok" && len(errs) == 0
	}
	tests := false
	if predicate && typecheck {
		tests = scopedTests(c, wt, t)
	}
	return map[string]any{
		"predicate": predicate, "typecheck": typecheck, "tests": tests,
		"pass": predicate && typecheck && tests,
	}
}

func renamedSym(r RenameSpec) string {
	if owner, _, ok := strings.Cut(r.Sym, "."); ok {
		return owner + "." + r.To
	}
	return r.To
}

func refCount(c config, wt, pkg, sym string) int {
	out := agoJSON(c, wt, "refs", "-p", pkg, "-s", sym)
	if out == nil || out["status"] != "ok" {
		return 0
	}
	n, _ := out["count"].(float64)
	return int(n)
}

// scopedTests runs the packages the ground-truth commit touched.
func scopedTests(c config, wt string, t Manifest) bool {
	repo := filepath.Join(c.scratch, t.Repo)
	out, err := exec.Command("git", "-C", repo, "diff", "--name-only", t.SHA+"^", t.SHA).Output()
	if err != nil {
		return false
	}
	dirs := map[string]bool{}
	for f := range strings.SplitSeq(string(out), "\n") {
		if strings.HasSuffix(f, ".go") {
			dirs["./"+filepath.Dir(f)] = true
		}
	}
	args := []string{"test", "-count=1", "-timeout", "10m"}
	for d := range dirs {
		args = append(args, d)
	}
	return run(wt, 15*time.Minute, "go", args...) == nil
}

func runAgent(ctx context.Context, c config, wt, prompt string) (string, error) {
	full := prompt + "\n\nMake this change across the whole repository. The task is complete when the change is applied everywhere and the project still typechecks."
	cmd := exec.CommandContext(ctx, "opencode", "run",
		"--agent", "bench", "-m", "local/"+c.model, "--format", "json", full)
	cmd.Dir = wt
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG="+filepath.Join(wt, "opencode.json"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeOpencodeConfig(b *testing.B, c config, wt, mode string) {
	b.Helper()
	tools := map[string]any{}
	mcp := map[string]any{}
	prompt := "You are completing a repository-wide Go refactoring task."
	if mode == "semantic" {
		for _, t := range []string{"bash", "edit", "write", "patch"} {
			tools[t] = false
		}
		mcp["ago"] = map[string]any{"type": "local", "enabled": true,
			"command": []string{c.agoBin, "mcp"}}
		prompt += " Use the ago_* tools for every inspection and code change: ago_refs to find usages, ago_rename to rename symbols, ago_set_body to change function bodies. Mutations are validated; a rejection tells you exactly what to fix. You cannot edit files directly."
	} else {
		prompt += " Use the shell and file editing tools; run go build to check your work."
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"local": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "local",
			"options": map[string]any{"baseURL": c.endpoint},
			"models":  map[string]any{c.model: map[string]any{"name": c.model}},
		}},
		"mcp": mcp,
		"agent": map[string]any{"bench": map[string]any{
			"mode": "primary", "prompt": prompt, "tools": tools,
		}},
		"permission": map[string]any{"edit": "allow", "bash": "allow", "webfetch": "deny"},
	}
	data, _ := json.MarshalIndent(cfg, "", " ")
	if err := os.WriteFile(filepath.Join(wt, "opencode.json"), data, 0o644); err != nil {
		b.Fatal(err)
	}
}

func worktree(b *testing.B, c config, t Manifest) string {
	b.Helper()
	repo := filepath.Join(c.scratch, t.Repo)
	wt := filepath.Join(b.TempDir(), t.Repo)
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "--force", wt, t.SHA+"^").CombinedOutput()
	if err != nil {
		b.Fatalf("worktree: %v\n%s", err, out)
	}
	return wt
}

func teardown(c config, t Manifest, wt string) {
	agoStop(c, wt)
	repo := filepath.Join(c.scratch, t.Repo)
	exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
}

func agoStop(c config, wt string) {
	exec.Command(c.agoBin, "stop", "-C", wt).Run()
}

func agoJSON(c config, wt string, args ...string) map[string]any {
	cmd := exec.Command(c.agoBin, append(args, "-C", wt)...)
	out, _ := cmd.Output()
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return nil
	}
	return m
}

func run(dir string, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func appendResult(path string, res map[string]any) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	res["ts"] = time.Now().Format(time.RFC3339)
	json.NewEncoder(f).Encode(res)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
