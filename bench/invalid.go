package bench

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// intermediateWatch measures the mechanism claim behind the bench: how
// often an arm leaves non-compiling code ON DISK at any point during an
// episode. Semantic mode should be zero by construction (every write is
// typechecked before it lands), raw mode passes through broken text
// states, and the oracle is the control. The watcher samples the worktree
// on an interval: when the .go/go.mod mtime fingerprint changes and then
// holds still for one interval (debounce, so mid-stream file writes are
// mostly not counted), it runs `go build ./...` and records the verdict.
// It runs identically for every arm, and the warm build cache keeps the
// check to no-op cost unless code actually changed, which is exactly when
// a verdict is owed.
type intermediateWatch struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	samples int
	invalid int
	firstAt time.Duration // zero until the first invalid sample
}

// watchIntermediates starts sampling wt; Stop ends it and reports.
func watchIntermediates(wt string, interval time.Duration) *intermediateWatch {
	w := &intermediateWatch{stop: make(chan struct{}), done: make(chan struct{})}
	start := time.Now()
	// The baseline is captured before returning, so a write the moment the
	// episode starts still reads as a change, never as the baseline.
	last := fingerprint(wt)
	go func() {
		defer close(w.done)
		pending := ""
		for {
			select {
			case <-w.stop:
				// The final state always gets its verdict, even when the
				// episode ended before a tick could sample it.
				if cur := fingerprint(wt); cur != last {
					w.record(wt, start)
				}
				return
			case <-time.After(interval):
			}
			cur := fingerprint(wt)
			switch {
			case cur == last:
				pending = ""
			case cur == pending:
				// Held still for one interval: sample it.
				w.record(wt, start)
				last, pending = cur, ""
			default:
				pending = cur
			}
		}
	}()
	return w
}

func (w *intermediateWatch) record(wt string, start time.Time) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = wt
	err := cmd.Run()
	w.mu.Lock()
	w.samples++
	if err != nil {
		w.invalid++
		if w.firstAt == 0 {
			w.firstAt = time.Since(start)
		}
	}
	w.mu.Unlock()
}

// Stop ends sampling and returns (states sampled, invalid states, seconds
// until the first invalid state; 0 when none).
func (w *intermediateWatch) Stop() (samples, invalid int, firstInvalidS float64) {
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.samples, w.invalid, w.firstAt.Seconds()
}

// fingerprint is a cheap mtime+size walk over the worktree's .go files and
// go.mod/go.sum; identical strings mean no relevant writes happened.
func fingerprint(wt string) string {
	var b strings.Builder
	filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// The agent cannot write .git, and vendor churn is not agent
			// editing; skipping both keeps the walk cheap on big repos.
			if name := d.Name(); name == ".git" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") && name != "go.mod" && name != "go.sum" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			b.WriteString(path)
			b.WriteByte('|')
			b.WriteString(info.ModTime().String())
			b.WriteByte('|')
			b.WriteString(strconv.FormatInt(info.Size(), 10))
			b.WriteByte('\n')
		}
		return nil
	})
	return b.String()
}
