package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The intermediate watcher measures the mechanism claim: how often an arm
// leaves non-compiling code on disk mid-episode. Write a broken state,
// then a fixed one; the watcher must count one invalid intermediate among
// the sampled states and report when it appeared.
func TestWatchIntermediates(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module tmpmod\n\ngo 1.24\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	if out, err := exec.Command("go", "build", "./...").Output(); err != nil && len(out) > 0 {
		t.Skip("go build unavailable")
	}

	w := watchIntermediates(dir, 50*time.Millisecond)
	write("broken.go", "package main\n\nfunc oops( {\n")
	time.Sleep(900 * time.Millisecond)
	write("broken.go", "package main\n\nfunc fine() {}\n")
	time.Sleep(900 * time.Millisecond)
	samples, invalid, firstInvalid := w.Stop()

	if samples < 2 {
		t.Fatalf("watcher sampled %d states, want at least the broken and fixed ones", samples)
	}
	if invalid < 1 {
		t.Errorf("broken intermediate not counted: samples=%d invalid=%d", samples, invalid)
	}
	if invalid >= samples {
		t.Errorf("fixed state counted as invalid: samples=%d invalid=%d", samples, invalid)
	}
	if firstInvalid <= 0 {
		t.Errorf("first_invalid_s not recorded: %v", firstInvalid)
	}
}

// A quiet episode (no writes) records zero samples and zero invalids:
// the metric never manufactures states.
func TestWatchIntermediatesQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module q\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := watchIntermediates(dir, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	samples, invalid, _ := w.Stop()
	if samples != 0 || invalid != 0 {
		t.Errorf("quiet worktree sampled: samples=%d invalid=%d", samples, invalid)
	}
}
