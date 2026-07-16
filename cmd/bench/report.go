package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// row is one (task, mode, profile) cell of the comparison table.
type row struct {
	Task, Mode, Profile string
	N, Passes, Capped   int
	MedianGreen         float64 // median wall_s of passing episodes
	Failures            map[string]int
	greens              []float64
}

// wilson is the 95% Wilson score interval for k passes out of n.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p := float64(k) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / denom
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / denom
	lo = math.Max(0, center-margin)
	hi = math.Min(1, center+margin)
	// Degenerate corners are exact, not estimated.
	if k == n {
		hi = 1
	}
	if k == 0 {
		lo = 0
	}
	return lo, hi
}

func aggregate(episodes []map[string]any) []row {
	byKey := map[string]*row{}
	for _, e := range episodes {
		task, _ := e["task"].(string)
		mode, _ := e["mode"].(string)
		profile, _ := e["profile"].(string)
		key := task + "|" + mode + "|" + profile
		r := byKey[key]
		if r == nil {
			r = &row{Task: task, Mode: mode, Profile: profile, Failures: map[string]int{}}
			byKey[key] = r
		}
		r.N++
		if pass, _ := e["pass"].(bool); pass {
			r.Passes++
			if w, ok := e["wall_s"].(float64); ok {
				r.greens = append(r.greens, w)
			}
		} else if fk, _ := e["failure_kind"].(string); fk != "" {
			r.Failures[fk]++
		}
		if capped, _ := e["capped"].(bool); capped {
			r.Capped++
		}
	}
	rows := make([]row, 0, len(byKey))
	for _, r := range byKey {
		sort.Float64s(r.greens)
		if n := len(r.greens); n > 0 {
			if n%2 == 1 {
				r.MedianGreen = r.greens[n/2]
			} else {
				r.MedianGreen = (r.greens[n/2-1] + r.greens[n/2]) / 2
			}
		}
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.Mode < b.Mode
	})
	return rows
}

func renderMarkdown(rows []row) string {
	var b strings.Builder
	b.WriteString("| profile | task | mode | pass@k | 95% CI | median green (s) | capped | failures |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		lo, hi := wilson(r.Passes, r.N)
		green := "-"
		if r.MedianGreen > 0 {
			green = fmt.Sprintf("%.0f", r.MedianGreen)
		}
		kinds := make([]string, 0, len(r.Failures))
		for k, n := range r.Failures {
			kinds = append(kinds, fmt.Sprintf("%s:%d", k, n))
		}
		sort.Strings(kinds)
		fmt.Fprintf(&b, "| %s | %s | %s | %d/%d | %.2f-%.2f | %s | %d | %s |\n",
			r.Profile, r.Task, r.Mode, r.Passes, r.N, lo, hi, green, r.Capped,
			strings.Join(kinds, " "))
	}
	return b.String()
}

// renderCurveCSV emits every passing episode's completion time, sorted,
// per (profile, mode): the completion-time curve the analysis plots.
func renderCurveCSV(rows []row) string {
	var b strings.Builder
	b.WriteString("profile,mode,task,wall_s\n")
	for _, r := range rows {
		for _, w := range r.greens {
			fmt.Fprintf(&b, "%s,%s,%s,%.1f\n", r.Profile, r.Mode, r.Task, w)
		}
	}
	return b.String()
}

func readEpisodes(runDirs []string) ([]map[string]any, error) {
	var out []map[string]any
	for _, dir := range runDirs {
		f, err := os.Open(filepath.Join(dir, "episodes.jsonl"))
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e map[string]any
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %w", dir, err)
			}
			out = append(out, e)
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func report(runDirs []string, curvePath string) error {
	episodes, err := readEpisodes(runDirs)
	if err != nil {
		return err
	}
	rows := aggregate(episodes)
	fmt.Print(renderMarkdown(rows))
	if curvePath != "" {
		if err := os.WriteFile(curvePath, []byte(renderCurveCSV(rows)), 0o644); err != nil {
			return err
		}
	}
	return nil
}
