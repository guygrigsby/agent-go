package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMLflow records the REST calls the exporter makes.
func fakeMLflow(t *testing.T, existing map[string]bool) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.URL.Path+" "+string(body))
		switch r.URL.Path {
		case "/api/2.0/mlflow/experiments/get-by-name":
			json.NewEncoder(w).Encode(map[string]any{"experiment": map[string]any{"experiment_id": "7"}})
		case "/api/2.0/mlflow/runs/search":
			var req struct {
				Filter string `json:"filter"`
			}
			json.Unmarshal(body, &req)
			runs := []any{}
			for tag := range existing {
				if len(req.Filter) > 0 && strings.Contains(req.Filter, tag) {
					runs = append(runs, map[string]any{"info": map[string]any{"run_id": "old"}})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		case "/api/2.0/mlflow/runs/create":
			json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"info": map[string]any{"run_id": "r1"}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func writeRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	episodes := `{"task":"traefik_1","mode":"semantic","profile":"glm","iter":0,"pass":true,"predicate":true,"typecheck":true,"tests":true,"wall_s":100.5,"tokens_in":1000,"tokens_out":50,"ts":"2026-07-16T12:00:00Z"}
{"task":"traefik_1","mode":"raw","profile":"glm","iter":0,"pass":false,"capped":true,"failure_kind":"capped","wall_s":720,"ts":"2026-07-16T12:20:00Z"}
`
	os.WriteFile(filepath.Join(dir, "episodes.jsonl"), []byte(episodes), 0o644)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"ago_rev":"abc123","profiles":[{"name":"glm"}]}`), 0o644)
	return dir
}

func TestExportMLflowCreatesRunPerEpisode(t *testing.T) {
	srv, calls := fakeMLflow(t, nil)
	dir := writeRunDir(t)
	if err := exportMLflow(srv.URL, []string{dir}); err != nil {
		t.Fatal(err)
	}
	creates, batches := 0, 0
	for _, c := range *calls {
		if strings.Contains(c, "runs/create") {
			creates++
		}
		if strings.Contains(c, "runs/log-batch") {
			batches++
			if !strings.Contains(c, `"key":"pass"`) && !strings.Contains(c, `"key":"wall_s"`) {
				t.Fatalf("log-batch missing metrics: %s", c)
			}
		}
	}
	if creates != 2 || batches != 2 {
		t.Fatalf("want 2 creates and 2 batches, got %d/%d\n%v", creates, batches, *calls)
	}
}

func TestExportMLflowIdempotent(t *testing.T) {
	dir := writeRunDir(t)
	tag := filepath.Base(dir) + "/traefik_1/semantic/0"
	srv, calls := fakeMLflow(t, map[string]bool{tag: true})
	if err := exportMLflow(srv.URL, []string{dir}); err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, c := range *calls {
		if strings.Contains(c, "runs/create") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("existing episode must be skipped: %d creates", creates)
	}
}
