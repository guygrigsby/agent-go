package bench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// canaryServer serves a fixed completion; after restartFile appears it
// serves the healthy answer, before that a corrupted one.
func canaryServer(t *testing.T, restartFile, healthy, corrupt string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := corrupt
		if _, err := os.Stat(restartFile); err == nil {
			content = healthy
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
}

func TestCanaryHealthy(t *testing.T) {
	srv := canaryServer(t, filepath.Join(t.TempDir(), "never"), "", "4")
	defer srv.Close()
	c := config{endpoint: srv.URL, model: "m",
		canary: canarySpec{Prompt: "2+2?", Want: "4"}}
	if err := ensureCanary(c); err != nil {
		t.Fatalf("healthy canary failed: %v", err)
	}
}

func TestCanaryRestartRecovers(t *testing.T) {
	restartFile := filepath.Join(t.TempDir(), "restarted")
	srv := canaryServer(t, restartFile, "4", "garbage")
	defer srv.Close()
	c := config{endpoint: srv.URL, model: "m",
		canary:     canarySpec{Prompt: "2+2?", Want: "4"},
		restartCmd: "touch " + restartFile}
	if err := ensureCanary(c); err != nil {
		t.Fatalf("canary did not recover after restart: %v", err)
	}
	if _, err := os.Stat(restartFile); err != nil {
		t.Fatal("restart cmd never ran")
	}
}

func TestCanaryStaysBrokenFails(t *testing.T) {
	srv := canaryServer(t, filepath.Join(t.TempDir(), "never"), "4", "garbage")
	defer srv.Close()
	c := config{endpoint: srv.URL, model: "m",
		canary:     canarySpec{Prompt: "2+2?", Want: "4"},
		restartCmd: "true"}
	if err := ensureCanary(c); err == nil {
		t.Fatal("broken server must fail the canary")
	}
}

func TestCanaryOffIsNoop(t *testing.T) {
	c := config{endpoint: "http://127.0.0.1:1"} // nothing listening
	if err := ensureCanary(c); err != nil {
		t.Fatalf("unconfigured canary must be a no-op: %v", err)
	}
}
