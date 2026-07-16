package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// canarySpec is a fixed probe with its recorded known-good answer: sent
// before each episode to catch a wedged-but-healthy-looking server (the
// GLM wedge bug) before it poisons a run. Loaded from the JSON file named
// by AGO_BENCH_CANARY: {"prompt": "...", "want": "..."}.
type canarySpec struct {
	Prompt string `json:"prompt"`
	Want   string `json:"want"`
}

// probeCanary sends the probe at temperature 0 and requires Want as a
// substring of the completion.
func probeCanary(endpoint, model string, spec canarySpec) error {
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"messages":    []any{map[string]any{"role": "user", "content": spec.Prompt}},
		"temperature": 0,
		"max_tokens":  64,
	})
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(strings.TrimRight(endpoint, "/")+"/chat/completions",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if len(out.Choices) == 0 {
		return fmt.Errorf("canary: empty choices")
	}
	if got := out.Choices[0].Message.Content; !strings.Contains(got, spec.Want) {
		return fmt.Errorf("canary mismatch: want %q in %q", spec.Want, got)
	}
	return nil
}

// canaryWithRestart probes once; on mismatch runs restartCmd and probes
// once more. A second failure is final: an unhealthy server invalidates
// every episode it would serve.
func canaryWithRestart(endpoint, model string, spec canarySpec, restartCmd string) error {
	err := probeCanary(endpoint, model, spec)
	if err == nil || restartCmd == "" {
		return err
	}
	if out, rerr := exec.Command("sh", "-c", restartCmd).CombinedOutput(); rerr != nil {
		return fmt.Errorf("canary failed (%v) and restart failed: %v\n%s", err, rerr, out)
	}
	return probeCanary(endpoint, model, spec)
}
