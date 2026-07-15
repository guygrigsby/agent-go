package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

// testEvent mirrors one line of `go test -json`'s event stream
// (see cmd/test2json). Package-level events (Test == "") carry build/setup
// output and the package's own aggregate pass/fail; per-test events
// (Test != "") are what TestRun reports.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// testResult is one reported test's outcome. Only leaf tests are reported
// (see the hasChild filter in TestRun): a table-driven test's own TestXxx
// event is redundant once its t.Run subtests are also reported — the
// subtest is where the actual failure message and source position live,
// the parent's own output is just "--- FAIL: TestXxx (0.00s)".
type testResult struct {
	Name    string  `json:"name"`
	Package string  `json:"package"`
	Pass    bool    `json:"pass"`
	Elapsed float64 `json:"elapsed_s"`
	Output  string  `json:"output,omitempty"`
}

// maxTestOutput bounds a failing test's captured output and a build-error
// Detail, per the spec.
const maxTestOutput = 2000

// testTimeout bounds the whole `go test` invocation, independent of the
// -timeout flag passed to the test binary itself (10m below): it also has
// to cover process start and go's own build step, and guarantees TestRun
// always returns rather than hanging the daemon forever if the go tool
// itself wedges.
const testTimeout = 15 * time.Minute

// TestRun executes `go test -json` scoped to pkg (an import path; empty
// means the whole workspace, "./...") with an optional -run filter, and
// returns structured per-test results.
//
// It does not hold s.mu: go test can run for minutes, and this only reads
// the workspace on disk through the real go toolchain — s.dir never changes
// after New, so reading it without the lock is safe. That said, the
// daemon's Accept loop (internal/daemon) serves one connection at a time,
// so a long test run still blocks other clients until it returns; holding
// no lock only matters once a concurrent-daemon task lands. Validation of
// mutations stays compiler-only (Patch); TestRun is the agent's separate
// behavior-verification loop, run at its own judgment per set of changes.
func (s *Snapshot) TestRun(pkg, run string) (map[string]any, error) {
	dir := s.dir
	target := pkg
	if target == "" {
		target = "./..."
	}
	args := []string{"test", "-count=1", "-timeout", "10m", "-json"}
	if run != "" {
		args = append(args, "-run", run)
	}
	args = append(args, target)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, &Reject{Reason: "tests could not run", Detail: err.Error()}
	}

	type accum struct {
		result testResult
		out    strings.Builder
	}
	byKey := map[string]*accum{}
	var order []string
	var buildOutput strings.Builder

	dec := json.NewDecoder(stdout)
	var parseErr error
	for {
		var ev testEvent
		if derr := dec.Decode(&ev); derr != nil {
			if derr != io.EOF {
				parseErr = derr
			}
			break
		}
		if ev.Test == "" {
			// Package-level output: for a build/setup failure this is where
			// the compiler's own error text lands (test-level events never
			// fire in that case).
			if ev.Action == "output" || ev.Action == "build-output" {
				boundedWrite(&buildOutput, ev.Output)
			}
			continue
		}
		key := ev.Package + "\x00" + ev.Test
		a, ok := byKey[key]
		if !ok {
			a = &accum{result: testResult{Name: ev.Test, Package: ev.Package}}
			byKey[key] = a
			order = append(order, key)
		}
		switch ev.Action {
		case "output":
			boundedWrite(&a.out, ev.Output)
		case "pass", "fail", "skip":
			a.result.Pass = ev.Action != "fail"
			a.result.Elapsed = ev.Elapsed
		}
	}

	// dec.Decode has consumed stdout to EOF (or a parse error) by this
	// point, so it's safe to Wait now — Wait must not run concurrently with
	// unfinished reads of the pipe it owns.
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil, &Reject{Reason: "tests could not run",
			Detail: "timed out after " + testTimeout.String()}
	}
	if parseErr != nil {
		return nil, &Reject{Reason: "tests could not run",
			Detail: "could not parse go test -json output: " + parseErr.Error() + "\n" + tail(stderr.String())}
	}

	// hasChild marks a parent TestXxx's own entry redundant once at least
	// one t.Run subtest of it was also reported.
	hasChild := map[string]bool{}
	for _, k := range order {
		a := byKey[k]
		prefix := a.result.Name + "/"
		for _, k2 := range order {
			b := byKey[k2]
			if b.result.Package == a.result.Package && strings.HasPrefix(b.result.Name, prefix) {
				hasChild[k] = true
				break
			}
		}
	}

	var results []testResult
	failed := 0
	for _, k := range order {
		if hasChild[k] {
			continue
		}
		r := byKey[k].result
		if !r.Pass {
			failed++
			out := byKey[k].out.String()
			if len(out) > maxTestOutput {
				out = tail(out)
			}
			r.Output = out
		}
		results = append(results, r)
	}

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		// Not a plain nonzero exit (e.g. the process vanished mid-run) —
		// always a Reject regardless of what parsed.
		return nil, &Reject{Reason: "tests could not run", Detail: waitErr.Error() + "\n" + tail(stderr.String())}
	}
	if exitErr != nil && failed == 0 {
		// A nonzero exit not explained by any parsed test failure is a
		// build or setup error. go's own exit codes for "tests failed" vs
		// "could not build" overlap (both commonly exit 1), so failure
		// evidence from the parsed stream is the reliable signal here, not
		// the exit code's literal value.
		detail := tail(buildOutput.String())
		if detail == "" {
			detail = tail(stderr.String())
		}
		return nil, &Reject{Reason: "tests could not run", Detail: detail}
	}

	return map[string]any{
		"status": "ok", "pass": failed == 0, "failed": failed, "tests": results,
	}, nil
}

// tail returns the last maxTestOutput bytes of s, for a bounded
// Reject.Detail.
func tail(s string) string {
	if len(s) <= maxTestOutput {
		return s
	}
	return s[len(s)-maxTestOutput:]
}

// boundedWrite appends s to b, maintaining tail semantics: when adding s
// would push the builder past 2*maxTestOutput bytes, the builder is first
// compacted to its last maxTestOutput bytes, then s is appended in full.
// The bound is event-bounded, not a hard cap: s always lands whole even
// when it exceeds maxTestOutput on its own, so the builder can hold up to
// 2*maxTestOutput plus the largest single event appended, not a flat
// 2*maxTestOutput.
func boundedWrite(b *strings.Builder, s string) {
	const cap = 2 * maxTestOutput
	if b.Len()+len(s) <= cap {
		b.WriteString(s)
		return
	}
	// Compaction needed: reset builder to its tail and re-append s.
	current := b.String()
	b.Reset()
	b.WriteString(tail(current))
	b.WriteString(s)
}
