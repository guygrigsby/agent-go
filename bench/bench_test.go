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
	endpoint, model, scratch, agoBin, results, runID string
	cap                                              time.Duration
	profile                                          Profile
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
	if name := os.Getenv("AGO_BENCH_PROFILE"); name != "" {
		p, err := loadProfile("profiles.json", name)
		if err != nil {
			b.Fatal(err)
		}
		c.profile = p
		c.endpoint, c.model = p.Endpoint, p.Model
	} else {
		// Bare env vars form an ad-hoc profile so every episode still
		// carries a profile name and run.json a full record.
		c.profile = Profile{Name: "adhoc", Endpoint: c.endpoint, Model: c.model}
	}
	if c.endpoint == "" || c.model == "" || c.scratch == "" {
		b.Skip("set AGO_BENCH_PROFILE (or AGO_BENCH_ENDPOINT + AGO_BENCH_MODEL) and AGO_BENCH_SCRATCH to run bench")
	}
	if v := os.Getenv("AGO_BENCH_CAP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			b.Fatalf("AGO_BENCH_CAP: %v", err)
		}
		c.cap = d
	}
	if c.results == "" {
		c.results = "results"
	}
	// Everything about a run is recorded under results/<runID> and belongs
	// in the repo: transcripts, configs, diffs, scores. Reproducibility is
	// part of the result.
	c.runID = time.Now().Format("20060102-150405")
	if err := os.MkdirAll(filepath.Join(c.results, c.runID), 0o755); err != nil {
		b.Fatal(err)
	}
	bin := filepath.Join(b.TempDir(), "ago")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/guygrigsby/agent-go/cmd/ago").CombinedOutput()
	if err != nil {
		b.Fatalf("build ago: %v\n%s", err, out)
	}
	c.agoBin = bin
	rev, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	meta := map[string]any{
		"endpoint": c.endpoint, "model": c.model, "cap": c.cap.String(),
		"ago_rev": strings.TrimSpace(string(rev)), "started": time.Now().Format(time.RFC3339),
		"raw_prompt_tokens":      estimateTokens(promptCommon + promptRaw),
		"semantic_prompt_tokens": estimateTokens(promptCommon + promptSemantic),
		"profile":                c.profile,
	}
	writeJSON(filepath.Join(c.results, c.runID, "run.json"), meta)
	return c
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", " ")
	os.WriteFile(path, append(data, '\n'), 0o644)
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
				for i := range b.N {
					if episode(b, c, t, mode, i) {
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
func episode(b *testing.B, c config, t Manifest, mode string, iter int) bool {
	b.Helper()
	b.StopTimer()
	wt := worktree(b, c, t)
	defer teardown(c, t, wt)
	epDir := filepath.Join(c.results, c.runID,
		fmt.Sprintf("%s_%s", t.Repo, t.SHA[:8]), mode, fmt.Sprint(iter))
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		b.Fatal(err)
	}
	writeOpencodeConfig(b, c, wt, mode)
	// Warm what any agent would find warm on a dev machine: module cache,
	// build cache, and (semantic mode) the daemon snapshot.
	run(wt, c.cap, "go", "mod", "download")
	run(wt, c.cap, "go", "build", "./...")
	baseline := map[string]int{}
	for _, r := range t.Renames {
		baseline[r.Pkg+"."+r.Sym] = refCount(c, wt, r.Pkg, r.Sym)
	}
	// Old parents can carry rot (test files that no longer typecheck).
	// Typecheck scoring is "no new errors", so capture the baseline set.
	baseErrs := errorSet(agoJSON(c, wt, "status"))
	if mode == "raw" {
		agoStop(c, wt) // raw mode gets no daemon; scoring respawns it later
	}

	b.StartTimer()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), c.cap)
	agentOut, agentErr := runAgent(ctx, c, wt, t.Prompt)
	timedOut := ctx.Err() == context.DeadlineExceeded
	cancel()
	wall := time.Since(start)
	b.StopTimer()

	res := score(c, wt, t, baseline, baseErrs)
	res["task"] = t.Repo + "_" + t.SHA[:8]
	res["mode"] = mode
	res["profile"] = c.profile.Name
	res["iter"] = iter
	res["prompt"] = t.Prompt
	res["wall_s"] = wall.Seconds()
	res["capped"] = timedOut
	pass0, _ := res["pass"].(bool)
	res["failure_kind"] = classifyFailure(agentErr, timedOut, agentOut, pass0)
	record(c, epDir, wt, agentOut, res)
	pass, _ := res["pass"].(bool)
	if !pass && testing.Verbose() {
		b.Logf("FAIL %s/%s: %v\nagent tail: %s", res["task"], mode, res, tail(agentOut, 800))
	}
	return pass
}

// record persists the full evidence for one episode: the agent transcript,
// the opencode config it ran under, the diff it produced, and the score.
func record(c config, epDir, wt, transcript string, res map[string]any) {
	os.WriteFile(filepath.Join(epDir, "transcript.jsonl"), []byte(transcript), 0o644)
	if cfg, err := os.ReadFile(filepath.Join(wt, "opencode.json")); err == nil {
		os.WriteFile(filepath.Join(epDir, "opencode.json"), cfg, 0o644)
	}
	diff, _ := exec.Command("git", "-C", wt, "diff").Output()
	os.WriteFile(filepath.Join(epDir, "changes.diff"), diff, 0o644)
	untracked, _ := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if len(untracked) > 0 {
		os.WriteFile(filepath.Join(epDir, "status.txt"), untracked, 0o644)
	}
	writeJSON(filepath.Join(epDir, "episode.json"), res)
	appendResult(filepath.Join(c.results, c.runID, "episodes.jsonl"), res)
}

func errorSet(status map[string]any) map[string]bool {
	set := map[string]bool{}
	if status == nil {
		return set
	}
	errs, _ := status["errors"].([]any)
	for _, e := range errs {
		if m, ok := e.(map[string]any); ok {
			set[fmt.Sprint(m["pos"])+"|"+fmt.Sprint(m["msg"])] = true
		}
	}
	return set
}

func score(c config, wt string, t Manifest, baseline map[string]int, baseErrs map[string]bool) map[string]any {
	predicate := true
	var specs []map[string]any
	for _, r := range t.Renames {
		oldRefs := refCount(c, wt, r.Pkg, r.Sym)
		newRefs := refCount(c, wt, r.Pkg, renamedSym(r))
		want := baseline[r.Pkg+"."+r.Sym]
		ok := oldRefs == 0 && newRefs == want
		specs = append(specs, map[string]any{
			"sym": r.Pkg + "." + r.Sym, "to": r.To, "ok": ok,
			"old_refs_left": oldRefs, "new_refs": newRefs, "baseline_refs": want,
		})
		if !ok {
			predicate = false
		}
	}
	typecheck := false
	var newErrs []string
	if out := agoJSON(c, wt, "status"); out != nil && out["status"] == "ok" {
		typecheck = true
		for e := range errorSet(out) {
			if !baseErrs[e] {
				newErrs = append(newErrs, e)
				typecheck = false
			}
		}
	}
	tests := false
	if predicate && typecheck {
		tests = scopedTests(c, wt, t)
	}
	return map[string]any{
		"predicate": predicate, "typecheck": typecheck, "tests": tests,
		"pass": predicate && typecheck && tests, "specs": specs,
		"new_errors": newErrs,
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
	cmd := exec.CommandContext(ctx, "opencode", "run", "--pure",
		"--agent", "bench", "-m", "local/"+c.model, "--format", "json", full)
	cmd.Dir = wt
	// Isolate from the user's global opencode config, skills, and plugins:
	// OPENCODE_CONFIG alone still merges ~/.config/opencode.
	xdg := filepath.Join(wt, ".xdg-config")
	os.MkdirAll(xdg, 0o755)
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG="+filepath.Join(wt, "opencode.json"),
		"XDG_CONFIG_HOME="+xdg)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeOpencodeConfig(b *testing.B, c config, wt, mode string) {
	b.Helper()
	// Single-agent purity in both modes: no subagent spawning, no user
	// skills. Round 3 autopsy: leaked global skills ate 36/45 tool calls of
	// one episode; task-spawned subagents dodge mode tool restrictions.
	tools := map[string]any{"task": false, "skill": false}
	mcp := map[string]any{}
	prompt := promptCommon
	if mode == "semantic" {
		// Pure protocol: no shell, no file reads, no text edits. The ago
		// tools are the only way to see or change code.
		for _, t := range []string{"bash", "edit", "write", "patch", "read", "grep", "glob", "list", "webfetch"} {
			tools[t] = false
		}
		mcp["ago"] = map[string]any{"type": "local", "enabled": true,
			"command": []string{c.agoBin, "mcp"}}
		prompt += promptSemantic
	} else {
		prompt += promptRaw
	}
	agent := map[string]any{"mode": "primary", "prompt": prompt, "tools": tools}
	// Temperature is the one sampler knob the OpenAI-compatible request can
	// carry; the rest of the profile's sampler block documents the server's
	// launch config and is recorded in run.json, not injected here.
	if temp, ok := c.profile.Sampler["temperature"]; ok {
		agent["temperature"] = temp
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{"local": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "local",
			"options": map[string]any{"baseURL": c.endpoint},
			"models":  map[string]any{c.model: map[string]any{"name": c.model}},
		}},
		"mcp":        mcp,
		"agent":      map[string]any{"bench": agent},
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
