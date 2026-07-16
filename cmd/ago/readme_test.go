// Drift guard for README.md: every ago invocation, the JSON rejection
// example, and the bench env vars shown in the README are checked against
// the code they describe. The verb and flag tables come straight from
// main.go (daemonOps, localOps, newFlagSet), the rejection shape from
// internal/snapshot and internal/daemon, the env vars from bench/ source.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/guygrigsby/agent-go/internal/snapshot"
)

func readme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(b)
}

type fence struct {
	info string // the ``` info string, e.g. "json" or ""
	body string
}

// fencedBlocks returns every ``` fenced code block in md.
func fencedBlocks(md string) []fence {
	var blocks []fence
	var cur *fence
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if cur == nil {
				cur = &fence{info: strings.TrimPrefix(trimmed, "```")}
			} else {
				blocks = append(blocks, *cur)
				cur = nil
			}
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	return blocks
}

var (
	// an ago invocation at the start of a line or after a shell pipe
	agoLine = regexp.MustCompile(`(?:^|\|)\s*ago\s+(.+)$`)
	// an inline code span invoking ago, e.g. `ago mcp`
	agoSpan = regexp.MustCompile("`ago ([^`]+)`")
)

// agoCommands extracts every ago invocation from the README: lines in
// fenced code blocks (including after a pipe) and inline code spans. The
// returned strings are everything after "ago ", trailing # comment
// stripped.
func agoCommands(md string) []string {
	var cmds []string
	for _, b := range fencedBlocks(md) {
		for _, line := range strings.Split(b.body, "\n") {
			if m := agoLine.FindStringSubmatch(line); m != nil {
				cmds = append(cmds, m[1])
			}
		}
	}
	for _, m := range agoSpan.FindAllStringSubmatch(md, -1) {
		cmds = append(cmds, m[1])
	}
	for i, c := range cmds {
		if idx := strings.Index(c, " #"); idx >= 0 {
			c = c[:idx]
		}
		cmds[i] = strings.TrimSpace(c)
	}
	return cmds
}

// commandProblems validates one extracted invocation against main.go's
// dispatch: the verb must be routed, every -flag must be declared in
// newFlagSet. Every ago flag is a string flag, so the token after a flag
// is its value and is skipped.
func commandProblems(cmd string) []string {
	tokens := strings.Fields(cmd)
	if len(tokens) == 0 {
		return []string{"empty command"}
	}
	var problems []string
	verb := tokens[0]
	if !slices.Contains(daemonOps, verb) && !slices.Contains(localOps, verb) {
		problems = append(problems, fmt.Sprintf("verb %q is not dispatched by main.go", verb))
	}
	fs, _ := newFlagSet(verb)
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			continue // positional arg, placeholder, or bare - (stdin)
		}
		name := strings.TrimLeft(tok, "-")
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		} else {
			i++ // string flag: next token is its value
		}
		if fs.Lookup(name) == nil {
			problems = append(problems, fmt.Sprintf("flag -%s is not declared in newFlagSet", name))
		}
	}
	return problems
}

func TestReadmeAgoCommands(t *testing.T) {
	cmds := agoCommands(readme(t))
	if len(cmds) < 10 {
		t.Fatalf("extracted only %d ago invocations from README.md; extraction is broken", len(cmds))
	}
	for _, cmd := range cmds {
		for _, p := range commandProblems(cmd) {
			t.Errorf("README invocation %q: %s", "ago "+cmd, p)
		}
	}
}

// TestCommandProblemsCatchDrift proves the guard bites: a verb main.go
// does not route and a flag newFlagSet does not declare must both be
// flagged.
func TestCommandProblemsCatchDrift(t *testing.T) {
	if p := commandProblems("frobnicate -p x"); len(p) == 0 {
		t.Error("unknown verb not caught")
	}
	if p := commandProblems("rename -p x -s Old -into New"); len(p) == 0 {
		t.Error("unknown flag not caught")
	}
	if p := commandProblems("rename -p x -s Old -to New"); len(p) != 0 {
		t.Errorf("valid command flagged: %v", p)
	}
}

// TestReadmeRejectionExample checks the JSON rejection example against the
// daemon's actual rejection shape: status/reason/diagnostics keys, status
// "rejected", and diagnostics entries that strict-decode into
// snapshot.Diagnostic (so a renamed pos/msg key fails here).
func TestReadmeRejectionExample(t *testing.T) {
	var jsonBlocks []string
	for _, b := range fencedBlocks(readme(t)) {
		if b.info == "json" {
			jsonBlocks = append(jsonBlocks, b.body)
		}
	}
	if len(jsonBlocks) == 0 {
		t.Fatal("no ```json block found in README.md")
	}
	for _, body := range jsonBlocks {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Errorf("README json block does not parse: %v", err)
			continue
		}
		if m["status"] != "rejected" {
			t.Errorf("README json block: status = %v, want \"rejected\"", m["status"])
		}
		for _, key := range []string{"reason", "diagnostics"} {
			if _, ok := m[key]; !ok {
				t.Errorf("README json block: missing %q key", key)
			}
		}
		raw, err := json.Marshal(m["diagnostics"])
		if err != nil {
			t.Fatalf("re-marshal diagnostics: %v", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var diags []snapshot.Diagnostic
		if err := dec.Decode(&diags); err != nil {
			t.Errorf("README diagnostics do not match snapshot.Diagnostic: %v", err)
			continue
		}
		for i, d := range diags {
			if d.Pos == "" || d.Msg == "" {
				t.Errorf("README diagnostic %d: pos/msg empty: %+v", i, d)
			}
		}
	}
}

// benchSource concatenates every .go file under bench/.
func benchSource(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "bench", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob bench/*.go: %v (%d files)", err, len(paths))
	}
	var sb strings.Builder
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		sb.Write(b)
	}
	return sb.String()
}

func TestReadmeBenchEnvVars(t *testing.T) {
	vars := regexp.MustCompile(`AGO_BENCH_[A-Z_]+`).FindAllString(readme(t), -1)
	if len(vars) == 0 {
		t.Fatal("no AGO_BENCH_* vars found in README.md")
	}
	src := benchSource(t)
	for _, v := range slices.Compact(slices.Sorted(slices.Values(vars))) {
		if !strings.Contains(src, v) {
			t.Errorf("README names %s but no bench/*.go file mentions it", v)
		}
	}
}

// TestReadmeBenchPattern checks that every -bench <pattern> shown in the
// README matches at least one Benchmark func in bench/, the way go test
// would.
func TestReadmeBenchPattern(t *testing.T) {
	patterns := regexp.MustCompile(`-bench\s+(\S+)`).FindAllStringSubmatch(readme(t), -1)
	if len(patterns) == 0 {
		t.Fatal("no -bench invocation found in README.md")
	}
	names := regexp.MustCompile(`func (Benchmark\w*)\(`).FindAllStringSubmatch(benchSource(t), -1)
	for _, m := range patterns {
		re, err := regexp.Compile(m[1])
		if err != nil {
			t.Errorf("README -bench %s is not a valid pattern: %v", m[1], err)
			continue
		}
		if !slices.ContainsFunc(names, func(n []string) bool { return re.MatchString(n[1]) }) {
			t.Errorf("README -bench %s matches no Benchmark func in bench/", m[1])
		}
	}
}
