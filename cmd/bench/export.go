package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// exportMLflow pushes each run dir's episodes to an MLflow tracking server
// as one MLflow run per episode, via the plain REST API (no SDK). Params
// carry the identity axes (task, mode, profile, iter, kind), metrics the
// numbers, tags the provenance. Idempotent: an episode is identified by
// its episode_path tag and skipped when already present, so re-exporting
// after new episodes land is safe.
func exportMLflow(uri string, runDirs []string) error {
	c := &mlflowClient{base: strings.TrimRight(uri, "/") + "/api/2.0/mlflow"}
	expID, err := c.experimentID("agent-go-bench")
	if err != nil {
		return err
	}
	kindByTask := taskKinds()
	exported, skipped := 0, 0
	for _, dir := range runDirs {
		episodes, err := readEpisodes([]string{dir})
		if err != nil {
			return err
		}
		var meta struct {
			AgoRev   string `json:"ago_rev"`
			Cap      string `json:"cap"`
			Profiles []struct {
				Name     string `json:"name"`
				Endpoint string `json:"endpoint"`
				Model    string `json:"model"`
				Quant    string `json:"quant"`
			} `json:"profiles"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
			json.Unmarshal(b, &meta)
		}
		profiles := map[string]map[string]string{}
		for _, p := range meta.Profiles {
			profiles[p.Name] = map[string]string{"model": p.Model, "endpoint": p.Endpoint, "quant": p.Quant}
		}
		runID := filepath.Base(dir)
		// Episodes recorded before the runner de-aliased the benchmark
		// sizing pass can repeat (task, mode, iter); suffix in-batch
		// collisions so every episode keeps its own MLflow run.
		seen := map[string]int{}
		for _, e := range episodes {
			path := fmt.Sprintf("%s/%v/%v/%v", runID, e["task"], e["mode"], e["iter"])
			if n := seen[path]; n > 0 {
				path = fmt.Sprintf("%s#%d", path, n+1)
			}
			seen[fmt.Sprintf("%s/%v/%v/%v", runID, e["task"], e["mode"], e["iter"])]++
			exists, err := c.runExists(expID, path)
			if err != nil {
				return err
			}
			if exists {
				skipped++
				continue
			}
			enrichEpisode(e, runID, meta.Cap, profiles, kindByTask)
			if err := c.exportEpisode(expID, path, meta.AgoRev, e); err != nil {
				return err
			}
			exported++
		}
	}
	fmt.Fprintf(os.Stderr, "%d exported, %d already present\n", exported, skipped)
	return nil
}

type mlflowClient struct {
	base string
	http http.Client
}

func (c *mlflowClient) call(method, path string, req, res any) error {
	body, _ := json.Marshal(req)
	r, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return fmt.Errorf("mlflow %s: %s", path, resp.Status)
	}
	if res != nil {
		return json.NewDecoder(resp.Body).Decode(res)
	}
	return nil
}

func (c *mlflowClient) experimentID(name string) (string, error) {
	var got struct {
		Experiment struct {
			ID string `json:"experiment_id"`
		} `json:"experiment"`
	}
	if err := c.call("GET", "/experiments/get-by-name?experiment_name="+name,
		map[string]any{"experiment_name": name}, &got); err == nil && got.Experiment.ID != "" {
		return got.Experiment.ID, nil
	}
	var created struct {
		ID string `json:"experiment_id"`
	}
	if err := c.call("POST", "/experiments/create", map[string]any{"name": name}, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *mlflowClient) runExists(expID, path string) (bool, error) {
	var got struct {
		Runs []any `json:"runs"`
	}
	err := c.call("POST", "/runs/search", map[string]any{
		"experiment_ids": []string{expID},
		"filter":         fmt.Sprintf("tags.episode_path = '%s'", path),
		"max_results":    1,
	}, &got)
	return len(got.Runs) > 0, err
}

// episode metric keys worth plotting; everything else identity-ish goes
// to params/tags.
var metricKeys = []string{"pass", "predicate", "typecheck", "tests", "capped",
	"wall_s", "tokens_in", "tokens_out", "tokens_reasoning", "cache_read",
	"cache_write", "steps", "rejects_total", "repairs_offered", "resends",
	"time_to_first_mutation_s", "pre_existing",
	"intermediate_states", "invalid_intermediates", "invalid_intermediate_rate",
	"first_invalid_s"}

func (c *mlflowClient) exportEpisode(expID, path, agoRev string, e map[string]any) error {
	start := time.Now().UnixMilli()
	if ts, _ := e["ts"].(string); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			start = t.UnixMilli()
		}
	}
	var created struct {
		Run struct {
			Info struct {
				ID string `json:"run_id"`
			} `json:"info"`
		} `json:"run"`
	}
	tags := []map[string]any{
		{"key": "episode_path", "value": path},
		{"key": "mlflow.runName", "value": path},
		{"key": "ago_rev", "value": agoRev},
	}
	if err := c.call("POST", "/runs/create", map[string]any{
		"experiment_id": expID, "start_time": start, "tags": tags,
	}, &created); err != nil {
		return err
	}
	runID := created.Run.Info.ID
	var metrics []map[string]any
	for _, k := range metricKeys {
		switch v := e[k].(type) {
		case float64:
			metrics = append(metrics, map[string]any{"key": k, "value": v, "timestamp": start, "step": 0})
		case bool:
			f := 0.0
			if v {
				f = 1.0
			}
			metrics = append(metrics, map[string]any{"key": k, "value": f, "timestamp": start, "step": 0})
		}
	}
	var params []map[string]any
	for _, k := range []string{"task", "mode", "profile", "iter", "failure_kind",
		"kind", "bench_run", "model", "endpoint", "quant", "cap", "no_repairs"} {
		if v, ok := e[k]; ok && fmt.Sprint(v) != "" {
			params = append(params, map[string]any{"key": k, "value": fmt.Sprint(v)})
		}
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i]["key"].(string) < metrics[j]["key"].(string) })
	if err := c.call("POST", "/runs/log-batch", map[string]any{
		"run_id": runID, "metrics": metrics, "params": params,
	}, nil); err != nil {
		return err
	}
	return c.call("POST", "/runs/update", map[string]any{
		"run_id": runID, "status": "FINISHED", "end_time": start + int64(1000*floatOr(e["wall_s"])),
	}, nil)
}

// enrichEpisode fills the identity axes older episode records lack, so
// every MLflow run is distinguishable: which bench run, which serving
// setup, which task kind.
func enrichEpisode(e map[string]any, runID, cap string, profiles map[string]map[string]string, kinds map[string]string) {
	e["bench_run"] = runID
	if cap != "" {
		e["cap"] = cap
	}
	if _, ok := e["kind"]; !ok {
		if k := kinds[fmt.Sprint(e["task"])]; k != "" {
			e["kind"] = k
		}
	}
	if p, ok := profiles[fmt.Sprint(e["profile"])]; ok {
		for k, v := range p {
			if v != "" {
				e[k] = v
			}
		}
	}
}

// taskKinds maps task ids to kinds from the mined manifests, for episodes
// recorded before the runner carried kind.
func taskKinds() map[string]string {
	out := map[string]string{}
	paths, _ := filepath.Glob("bench/tasks-*.json")
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var tasks []struct {
			Repo string `json:"repo"`
			SHA  string `json:"sha"`
			Kind string `json:"kind"`
		}
		if json.Unmarshal(b, &tasks) != nil {
			continue
		}
		for _, t := range tasks {
			if len(t.SHA) >= 8 {
				k := t.Kind
				if k == "" {
					k = "rename"
				}
				out[t.Repo+"_"+t.SHA[:8]] = k
			}
		}
	}
	return out
}

func floatOr(v any) float64 {
	f, _ := v.(float64)
	return f
}
