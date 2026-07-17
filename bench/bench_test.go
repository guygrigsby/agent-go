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
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

type config struct {
	endpoint, model, scratch, agoBin, results, runID string
	cap                                              time.Duration
	profile                                          Profile
	profiles                                         []Profile
	canary                                           canarySpec
	restartCmd                                       string
	modes                                            []string
	reqLog                                           string // per-episode daemon request log path
}

// agoEnv is the environment for every command that may spawn the ago
// daemon, carrying the per-episode request-log path.
func agoEnv(c config) []string {
	env := os.Environ()
	if c.reqLog != "" {
		env = append(env, "AGO_LOG_REQUESTS="+c.reqLog)
	}
	return env
}

// ensureCanary is a no-op until AGO_BENCH_CANARY configures a probe.
func ensureCanary(c config) error {
	if c.canary.Prompt == "" {
		return nil
	}
	return canaryWithRestart(c.endpoint, c.model, c.canary, c.restartCmd)
}

func setup(b testing.TB) config {
	b.Helper()
	c := config{
		endpoint: os.Getenv("AGO_BENCH_ENDPOINT"),
		model:    os.Getenv("AGO_BENCH_MODEL"),
		scratch:  os.Getenv("AGO_BENCH_SCRATCH"),
		results:  os.Getenv("AGO_BENCH_RESULTS"),
		cap:      15 * time.Minute,
	}
	if csv := os.Getenv("AGO_BENCH_PROFILES"); csv != "" {
		ps, err := loadProfiles("profiles.json", csv)
		if err != nil {
			b.Fatal(err)
		}
		c.profiles = ps
	} else if name := os.Getenv("AGO_BENCH_PROFILE"); name != "" {
		p, err := loadProfile("profiles.json", name)
		if err != nil {
			b.Fatal(err)
		}
		c.profiles = []Profile{p}
	} else {
		// Bare env vars form an ad-hoc profile so every episode still
		// carries a profile name and run.json a full record.
		c.profiles = []Profile{{Name: "adhoc", Endpoint: c.endpoint, Model: c.model}}
	}
	c.profile = c.profiles[0]
	c.endpoint, c.model = c.profile.Endpoint, c.profile.Model
	c.modes = modesFor(os.Getenv("AGO_BENCH_MODES"))
	modelNeeded := slices.Contains(c.modes, "raw") || slices.Contains(c.modes, "semantic")
	if c.scratch == "" || (modelNeeded && (c.endpoint == "" || c.model == "")) {
		b.Skip("set AGO_BENCH_PROFILE (or AGO_BENCH_ENDPOINT + AGO_BENCH_MODEL) and AGO_BENCH_SCRATCH to run bench; oracle-only runs need only AGO_BENCH_SCRATCH")
	}
	if path := os.Getenv("AGO_BENCH_CANARY"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := json.Unmarshal(raw, &c.canary); err != nil {
			b.Fatalf("AGO_BENCH_CANARY: %v", err)
		}
		c.restartCmd = os.Getenv("AGO_BENCH_RESTART_CMD")
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
	// part of the result. AGO_BENCH_RESUME=<runID> reuses an interrupted
	// run's dir; recorded episodes are skipped (resumeEpisode), so a
	// thermal pause costs nothing but the wall clock.
	c.runID = time.Now().Format("20060102-150405")
	if r := os.Getenv("AGO_BENCH_RESUME"); r != "" {
		c.runID = r
	}
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
		"profiles":               c.profiles,
	}
	if os.Getenv("AGO_NO_REPAIRS") != "" {
		meta["no_repairs"] = true
	}
	// A resumed run keeps its original run.json (its identity); the resume
	// leg records itself alongside, rev and all, so a mid-round engine
	// change is visible in the evidence.
	if _, err := os.Stat(filepath.Join(c.results, c.runID, "run.json")); err == nil {
		writeJSON(filepath.Join(c.results, c.runID, "run-resume-"+time.Now().Format("150405")+".json"), meta)
	} else {
		writeJSON(filepath.Join(c.results, c.runID, "run.json"), meta)
	}
	return c
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", " ")
	os.WriteFile(path, append(data, '\n'), 0o644)
}

// loadTasks reads every mined manifest file; kind dispatch does the rest.
func loadTasks(b testing.TB) []Manifest {
	b.Helper()
	paths, err := filepath.Glob("tasks-*.json")
	if err != nil || len(paths) == 0 {
		b.Fatalf("no task manifests: %v", err)
	}
	var tasks []Manifest
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			b.Fatal(err)
		}
		var batch []Manifest
		if err := json.Unmarshal(raw, &batch); err != nil {
			b.Fatalf("%s: %v", p, err)
		}
		tasks = append(tasks, batch...)
	}
	return tasks
}

// TestOracleSweep replays ground truth through ago for every runnable
// task, in parallel: oracle episodes have no shared endpoint, so the only
// limits are cores and memory (each episode loads its repo's typechecked
// snapshot). Bound with -parallel; vault and boundary snapshots are heavy.
//
//	AGO_BENCH_MODES=oracle AGO_BENCH_SCRATCH=<clones> \
//	go test ./bench -run OracleSweep -parallel 4 -timeout 0 -v
func TestOracleSweep(t *testing.T) {
	if !slices.Contains(modesFor(os.Getenv("AGO_BENCH_MODES")), "oracle") {
		t.Skip("set AGO_BENCH_MODES=oracle to run the oracle sweep")
	}
	c := setup(t)
	for _, task := range loadTasks(t) {
		if !task.HasSpecs() {
			continue
		}
		t.Run(fmt.Sprintf("%s_%s", task.Repo, task.SHA[:8]), func(t *testing.T) {
			t.Parallel()
			if !episode(t, c, task, "oracle", 0) {
				t.Errorf("oracle failed %s %s: see episode.json", task.Kind, task.SHA[:8])
			}
		})
	}
}

func BenchmarkRename(b *testing.B) {
	c := setup(b)
	// AGO_BENCH_SUITE=smoke runs one certified task per kind from the
	// smallest repo; anything else is the full roster.
	tasks := suiteTasks(loadTasks(b), os.Getenv("AGO_BENCH_SUITE"))
	// The harness runs each sub-benchmark twice (a sizing pass with N=1,
	// then the requested N), so a loop index would reuse iter 0 and alias
	// two different episodes' identity and evidence dirs. A persistent
	// per-cell counter keeps every episode unique.
	seq := map[string]int{}
	for _, p := range c.profiles {
		c := c
		c.profile, c.endpoint, c.model = p, p.Endpoint, p.Model
		for _, t := range tasks {
			if !t.EligibleForModel() {
				if t.HasSpecs() {
					b.Logf("skipping %s_%s: specs present but not oracle certified; run the oracle sweep then `bench certify` (tenet 2)", t.Repo, t.SHA[:8])
				}
				continue
			}
			for _, mode := range c.modes {
				cell := fmt.Sprintf("%s/%s_%s/%s", p.Name, t.Repo, t.SHA[:8], mode)
				b.Run(cell, func(b *testing.B) {
					passes := 0
					for range b.N {
						iter := seq[cell]
						seq[cell]++
						if episode(b, c, t, mode, iter) {
							passes++
						}
					}
					b.ReportMetric(float64(passes)/float64(b.N), "pass")
				})
			}
		}
	}
}

// episode runs one agent attempt and scores it. Only the agent run is on
// the clock; worktree setup, cache warming, and scoring are not.
func episode(b testing.TB, c config, t Manifest, mode string, iter int) bool {
	b.Helper()
	// Benchmarks time only the agent run; parallel test sweeps (oracle)
	// have no benchmark timer.
	stopTimer, startTimer := func() {}, func() {}
	if bb, ok := b.(*testing.B); ok {
		stopTimer, startTimer = bb.StopTimer, bb.StartTimer
	}
	stopTimer()
	epDirEarly := filepath.Join(c.results, c.runID,
		fmt.Sprintf("%s_%s", t.Repo, t.SHA[:8]), mode, fmt.Sprint(iter))
	if res, ok := resumeEpisode(epDirEarly); ok {
		b.Logf("resume: %s already recorded, skipping", epDirEarly)
		startTimer()
		return res["pass"] == true
	}
	wt := worktree(b, c, t)
	defer teardown(c, t, wt)
	epDir := filepath.Join(c.results, c.runID,
		fmt.Sprintf("%s_%s", t.Repo, t.SHA[:8]), mode, fmt.Sprint(iter))
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		b.Fatal(err)
	}
	// Every daemon this episode spawns (warm-up, agent, scoring) gets the
	// log path on its own command environment, so episodes can run in
	// parallel. Scorer queries land in the same file; agent_started and
	// agent_done in episode.json bound the agent's window.
	if absLog, err := filepath.Abs(filepath.Join(epDir, "requests.jsonl")); err == nil {
		c.reqLog = absLog
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

	if mode != "oracle" {
		if err := ensureCanary(c); err != nil {
			b.Fatalf("server failed canary before episode: %v", err)
		}
	}

	startTimer()
	start := time.Now()
	var agentOut string
	var agentErr error
	var timedOut bool
	if mode == "oracle" {
		agentOut, agentErr = runOracle(c, wt, t)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), c.cap)
		agentOut, agentErr = runAgent(ctx, c, wt, t.Prompt)
		timedOut = ctx.Err() == context.DeadlineExceeded
		cancel()
	}
	wall := time.Since(start)
	stopTimer()

	res := score(b, c, wt, t, baseline, baseErrs)
	res["task"] = t.Repo + "_" + t.SHA[:8]
	kind := t.Kind
	if kind == "" {
		kind = "rename"
	}
	res["kind"] = kind
	if os.Getenv("AGO_NO_REPAIRS") != "" {
		res["no_repairs"] = true
	}
	res["mode"] = mode
	res["profile"] = c.profile.Name
	res["iter"] = iter
	res["prompt"] = t.Prompt
	res["wall_s"] = wall.Seconds()
	tok := sumTokens(agentOut)
	res["tokens_in"] = tok.In
	res["tokens_out"] = tok.Out
	res["tokens_reasoning"] = tok.Reasoning
	res["cache_read"] = tok.CacheRead
	res["cache_write"] = tok.CacheWrite
	res["steps"] = tok.Steps
	res["agent_started"] = start.UTC().Format(time.RFC3339Nano)
	res["agent_done"] = start.Add(wall).UTC().Format(time.RFC3339Nano)
	res["capped"] = timedOut
	pass0, _ := res["pass"].(bool)
	res["failure_kind"] = classifyFailure(agentErr, timedOut, agentOut, pass0)
	if mode == "oracle" && agentErr != nil {
		// An oracle rejection is a finding about the task or the protocol,
		// not a harness failure.
		res["failure_kind"] = "oracle_reject"
		res["oracle_error"] = agentErr.Error()
	}
	// Fold the daemon request log into the episode evidence before record
	// writes episode.json and episodes.jsonl, so counters land in both.
	// The window [start, start+wall] keeps agent traffic and drops the
	// warm-up and scorer queries sharing the same log file.
	if lines := readRequestLog(filepath.Join(epDir, "requests.jsonl")); lines != nil {
		for k, v := range requestCounters(lines, start, start.Add(wall)) {
			res[k] = v
		}
	}
	record(c, epDir, wt, agentOut, res)
	pass, _ := res["pass"].(bool)
	if !pass && testing.Verbose() {
		b.Logf("FAIL %s/%s: %v\nagent tail: %s", res["task"], mode, res, tail(agentOut, 800))
	}
	return pass
}

// runOracle replays the ground-truth change through ago itself: proves the
// task is protocol-solvable, records the time-to-green floor, and leaves a
// transcript of accepted calls (the SFT corpus seed). Any rejection is an
// oracle finding: either the spec is wrong or the protocol has a gap.
func runOracle(c config, wt string, t Manifest) (string, error) {
	var b strings.Builder
	call := func(args ...string) (map[string]any, error) {
		out := agoJSON(c, wt, args...)
		rec, _ := json.Marshal(map[string]any{"call": args, "res": out})
		b.Write(rec)
		b.WriteByte('\n')
		status, _ := out["status"].(string)
		if status != "accepted" {
			return out, fmt.Errorf("oracle call %v: %v", args, out)
		}
		return out, nil
	}
	switch t.Kind {
	case "", "rename":
		for _, r := range t.Renames {
			if _, err := call("rename", "-p", r.Pkg, "-s", r.Sym, "--to", r.To); err != nil {
				return b.String(), err
			}
		}
	case "add-param":
		for _, a := range t.AddParams {
			if out, err := call("add-param", "-p", a.Pkg, "-s", a.Sym,
				"--name", a.Name, "--type", a.Type, "--default", zeroExpr(a.Type)); err != nil {
				// Sequential replay hit a shape one add_param cannot express
				// (an interface widened with its implementors, or a function
				// registered as a value). Fall back to composing the whole
				// change as one atomic patch.
				env := composeEnv{wt: wt, mod: moduleOf(wt),
					call: func(args ...string) map[string]any {
						out := agoJSON(c, wt, args...)
						rec, _ := json.Marshal(map[string]any{"call": args, "res": out})
						b.Write(rec)
						b.WriteByte('\n')
						return out
					},
					patch: func(env map[string]any) map[string]any {
						body, _ := json.Marshal(env)
						out := agoJSONStdin(c, wt, string(body), "patch", "--body-file", "-")
						rec, _ := json.Marshal(map[string]any{"call": []any{"patch", env}, "res": out})
						b.Write(rec)
						b.WriteByte('\n')
						return out
					}}
				if cerr := composeAddParam(env, t, out); cerr != nil {
					return b.String(), cerr
				}
				return b.String(), nil
			}
		}
	case "move":
		// Movers sharing a source and target batch into one op: intra-set
		// dependencies (a type, its constructor, its tests) are legal
		// inside a batch where sequential single moves would reject.
		// Compound (renamed) movers still go one at a time, move then
		// rename. A full pass of deferrals with no acceptance is a genuine
		// blocker.
		type cell struct{ pkg, toPkg string }
		batches := map[cell][]string{}
		var queue []MoveSpec
		for _, m := range t.Moves {
			if m.ToName == "" {
				batches[cell{m.Pkg, m.ToPkg}] = append(batches[cell{m.Pkg, m.ToPkg}], m.Sym)
			} else {
				queue = append(queue, m)
			}
		}
		cells := make([]cell, 0, len(batches))
		for bc := range batches {
			cells = append(cells, bc)
		}
		sort.Slice(cells, func(i, j int) bool {
			return cells[i].pkg+cells[i].toPkg < cells[j].pkg+cells[j].toPkg
		})
		submit := func(env map[string]any) map[string]any {
			body, _ := json.Marshal(env)
			out := agoJSONStdin(c, wt, string(body), "patch", "--body-file", "-")
			rec, _ := json.Marshal(map[string]any{"call": []string{"patch", string(body)}, "res": out})
			b.Write(rec)
			b.WriteByte('\n')
			return out
		}
		moveOp := func(bc cell) map[string]any {
			// create_pkg unconditionally: the oracle replays ground truth,
			// and the commit either created the target package or it
			// already existed (the flag is a no-op then).
			syms := batches[bc]
			sort.Strings(syms)
			return map[string]any{"op": "move_decl", "pkg": bc.pkg, "syms": syms,
				"to_pkg": bc.toPkg, "create_pkg": true}
		}
		for i, bc := range cells {
			out := submit(map[string]any{"pkg": bc.pkg, "ops": []map[string]any{moveOp(bc)}})
			if status, _ := out["status"].(string); status == "accepted" {
				continue
			} else if len(queue) > 0 {
				// v1 ceiling: reconcile and compound (renamed) movers do not
				// mix; the rename would fight the post-state upserts.
				return b.String(), fmt.Errorf("oracle batched move %s -> %s: %v", bc.pkg, bc.toPkg, out)
			}
			// The pure move rejected, so the commit authored more than a
			// relocation (a dropped parameter, a new helper, an import the
			// target may not hold). Retry as ONE atomic patch: any leading
			// file deletes, the remaining batched moves (minus movers those
			// deletes excised — their rewritten post-state upserts instead),
			// then the commit's decl-level rewrites; the end-of-list
			// typecheck is the arbiter. Cells accepted before this one keep
			// their plain moves; the plan only shapes what is still pending.
			plan, rerr := reconcileOps(filepath.Join(c.scratch, t.Repo), t.SHA, moduleOf(wt), t.Moves)
			if rerr != nil {
				return b.String(), fmt.Errorf("oracle reconcile %s: %w", t.SHA[:8], rerr)
			}
			for _, n := range plan.notes {
				fmt.Fprintf(&b, `{"reconcile_note":%q}`+"\n", n)
			}
			ops := append([]map[string]any{}, plan.preOps...)
			for _, rc := range cells[i:] {
				syms := make([]string, 0, len(batches[rc]))
				for _, sym := range batches[rc] {
					if !plan.dropMovers[rc.pkg+"|"+sym] {
						syms = append(syms, sym)
					}
				}
				if len(syms) == 0 {
					continue
				}
				sort.Strings(syms)
				ops = append(ops, map[string]any{"op": "move_decl", "pkg": rc.pkg,
					"syms": syms, "to_pkg": rc.toPkg, "create_pkg": true})
			}
			ops = append(ops, plan.ops...)
			out = submit(map[string]any{"pkg": bc.pkg, "ops": ops})
			if status, _ := out["status"].(string); status != "accepted" {
				return b.String(), fmt.Errorf("oracle move+reconcile %s -> %s: %v", bc.pkg, bc.toPkg, out)
			}
			break
		}
		deferred := 0
		for len(queue) > 0 {
			m := queue[0]
			queue = queue[1:]
			ops := []map[string]any{
				{"op": "move_decl", "sym": m.Sym, "to_pkg": m.ToPkg, "create_pkg": true}}
			env, _ := json.Marshal(map[string]any{"pkg": m.Pkg, "ops": ops})
			out := agoJSONStdin(c, wt, string(env), "patch", "--body-file", "-")
			rec, _ := json.Marshal(map[string]any{"call": []string{"patch", string(env)}, "res": out})
			b.Write(rec)
			b.WriteByte('\n')
			if status, _ := out["status"].(string); status != "accepted" {
				reason, _ := out["reason"].(string)
				if strings.Contains(reason, "depends on package-local symbols") && deferred <= len(queue) {
					queue = append(queue, m)
					deferred++
					continue
				}
				return b.String(), fmt.Errorf("oracle move %s.%s: %v", m.Pkg, m.Sym, out)
			}
			deferred = 0
			if m.ToName != "" {
				// Compound spec: the ground truth renamed on the way; a
				// second patch renames in the target (rename computes its
				// edits against the live snapshot, so it cannot share a
				// patch with the move that creates its subject).
				renv, _ := json.Marshal(map[string]any{"pkg": m.ToPkg, "ops": []map[string]any{
					{"op": "rename", "sym": m.Sym, "to": m.ToName}}})
				rout := agoJSONStdin(c, wt, string(renv), "patch", "--body-file", "-")
				rec, _ := json.Marshal(map[string]any{"call": []string{"patch", string(renv)}, "res": rout})
				b.Write(rec)
				b.WriteByte('\n')
				if status, _ := rout["status"].(string); status != "accepted" {
					return b.String(), fmt.Errorf("oracle move rename %s -> %s: %v", m.Sym, m.ToName, rout)
				}
			}
		}
	default:
		return "", fmt.Errorf("oracle has no replay for kind %q", t.Kind)
	}
	return b.String(), nil
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

// predicateFn decides whether the workspace satisfies a task's goal and
// reports the per-spec evidence. One per task kind; new kinds add an entry
// here and an extractor in cmd/bench, never a scorer fork.
type predicateFn func(c config, wt string, t Manifest, baseline map[string]int) (bool, []map[string]any)

var predicates = map[string]predicateFn{
	"": renamePredicate, "rename": renamePredicate,
	"add-param": addParamPredicate,
	"move":      movePredicate,
}

func movePredicate(c config, wt string, t Manifest, _ map[string]int) (bool, []map[string]any) {
	predicate := len(t.Moves) > 0
	var specs []map[string]any
	for _, m := range t.Moves {
		// A compound spec lands under its new name in the target.
		want := m.Sym
		if m.ToName != "" {
			want = m.ToName
		}
		inTarget := inspectOK(c, wt, m.ToPkg, want)
		inSource := inspectOK(c, wt, m.Pkg, m.Sym)
		ok := inTarget && !inSource
		specs = append(specs, map[string]any{
			"sym": m.Pkg + "." + m.Sym, "to_pkg": m.ToPkg, "to_name": m.ToName, "ok": ok,
			"in_target": inTarget, "still_in_source": inSource,
		})
		if !ok {
			predicate = false
		}
	}
	return predicate, specs
}

func inspectOK(c config, wt, pkg, sym string) bool {
	out := agoJSON(c, wt, "inspect", "-p", pkg, "-s", sym)
	return out != nil && out["status"] == "ok"
}

func addParamPredicate(c config, wt string, t Manifest, _ map[string]int) (bool, []map[string]any) {
	predicate := len(t.AddParams) > 0
	var specs []map[string]any
	for _, a := range t.AddParams {
		sig := ""
		if out := agoJSON(c, wt, "inspect", "-p", a.Pkg, "-s", a.Sym); out != nil && out["status"] == "ok" {
			sig, _ = out["type"].(string)
		}
		ok := sigHasParam(sig, a.Name, a.Type)
		specs = append(specs, map[string]any{
			"sym": a.Pkg + "." + a.Sym, "param": a.Name + " " + a.Type,
			"ok": ok, "signature": sig,
		})
		if !ok {
			predicate = false
		}
	}
	return predicate, specs
}

func predicateFor(kind string) predicateFn { return predicates[kind] }

func renamePredicate(c config, wt string, t Manifest, baseline map[string]int) (bool, []map[string]any) {
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
	return predicate, specs
}

// passRule: predicate and typecheck are absolute; the tests gate counts
// only when the same scoped tests pass on a pristine worktree — a failing
// baseline (docker deps, parent rot) makes the gate vacuous, since not
// even the ground truth could clear it here.
func passRule(predicate, typecheck, tests, baselineTests bool) bool {
	return predicate && typecheck && (tests || !baselineTests)
}

func score(b testing.TB, c config, wt string, t Manifest, baseline map[string]int, baseErrs map[string]bool) map[string]any {
	fn := predicateFor(t.Kind)
	if fn == nil {
		return map[string]any{"predicate": false, "typecheck": false, "tests": false,
			"pass": false, "unknown_kind": t.Kind}
	}
	predicate, specs := fn(c, wt, t, baseline)
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
	baselineTests := true
	if predicate && typecheck {
		tests = scopedTests(c, wt, t)
		if !tests {
			// Lazy baseline: only a failing gate pays for the pristine
			// worktree that decides whether the gate could pass at all.
			pw := worktree(b, c, t)
			defer teardown(c, t, pw)
			run(pw, c.cap, "go", "mod", "download")
			baselineTests = scopedTests(c, pw, t)
		}
	}
	return map[string]any{
		"predicate": predicate, "typecheck": typecheck, "tests": tests,
		"tests_baseline": baselineTests,
		"pass":           passRule(predicate, typecheck, tests, baselineTests), "specs": specs,
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
	cmd.Env = append(agoEnv(c),
		"OPENCODE_CONFIG="+filepath.Join(wt, "opencode.json"),
		"XDG_CONFIG_HOME="+xdg)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeOpencodeConfig(b testing.TB, c config, wt, mode string) {
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

func worktree(b testing.TB, c config, t Manifest) string {
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
	cmd := exec.Command(c.agoBin, "daemon", "stop", "-C", wt)
	cmd.Env = agoEnv(c)
	cmd.Run()
}

func agoJSONStdin(c config, wt, stdin string, args ...string) map[string]any {
	cmd := exec.Command(c.agoBin, append(args, "-C", wt)...)
	cmd.Env = agoEnv(c)
	cmd.Stdin = strings.NewReader(stdin)
	out, _ := cmd.Output()
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return nil
	}
	return m
}

func agoJSON(c config, wt string, args ...string) map[string]any {
	cmd := exec.Command(c.agoBin, append(args, "-C", wt)...)
	cmd.Env = agoEnv(c)
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
