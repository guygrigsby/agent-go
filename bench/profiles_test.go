package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`{
		"glm-flash": {
			"endpoint": "http://rig:8080/v1",
			"model": "glm-4.7-flash",
			"sampler": {"temperature": 0.7, "top_p": 0.95, "repeat_penalty": 1.0},
			"quant": "Q4_K_M",
			"context": 65536
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := loadProfile(path, "glm-flash")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "glm-flash" || p.Endpoint != "http://rig:8080/v1" || p.Model != "glm-4.7-flash" {
		t.Fatalf("wrong profile: %+v", p)
	}
	if p.Sampler["repeat_penalty"] != 1.0 {
		t.Fatalf("sampler not carried: %+v", p.Sampler)
	}
	if _, err := loadProfile(path, "nope"); err == nil {
		t.Fatal("unknown profile must error")
	}
}

// The committed profiles.json must always parse and its entries carry the
// two fields the runner substitutes into config.
func TestShippedProfilesLoad(t *testing.T) {
	p, err := loadProfile("profiles.json", "glm-flash")
	if err != nil {
		t.Fatal(err)
	}
	if p.Endpoint == "" || p.Model == "" {
		t.Fatalf("incomplete profile: %+v", p)
	}
	if rp, ok := p.Sampler["repeat_penalty"]; !ok || rp != 1.0 {
		t.Fatalf("repeat_penalty must be pinned to 1.0, got %v", rp)
	}
}
