package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Profile pins everything about how a model is served for a run: one named
// entry per model in bench/profiles.json. Sampler values the OpenAI API
// cannot carry per-request (top_k, repeat_penalty) document the server's
// launch config; temperature is injected into the agent config. The whole
// profile is embedded in run.json so a result is never separated from the
// serving setup that produced it.
type Profile struct {
	Name     string         `json:"name,omitempty"`
	Endpoint string         `json:"endpoint"`
	Model    string         `json:"model"`
	Sampler  map[string]any `json:"sampler,omitempty"`
	Thinking string         `json:"thinking,omitempty"`
	Quant    string         `json:"quant,omitempty"`
	Context  int            `json:"context,omitempty"`
	Notes    string         `json:"notes,omitempty"`
}

// loadProfiles resolves a comma-separated list of profile names; any
// unknown name fails the whole list.
func loadProfiles(path, csv string) ([]Profile, error) {
	var out []Profile
	for _, name := range strings.Split(csv, ",") {
		p, err := loadProfile(path, strings.TrimSpace(name))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func loadProfile(path, name string) (Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var all map[string]Profile
	if err := json.Unmarshal(b, &all); err != nil {
		return Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	p, ok := all[name]
	if !ok {
		known := make([]string, 0, len(all))
		for n := range all {
			known = append(known, n)
		}
		sort.Strings(known)
		return Profile{}, fmt.Errorf("profile %q not in %s (have %v)", name, path, known)
	}
	p.Name = name
	return p, nil
}
