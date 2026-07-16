package daemon

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/guygrigsby/agent-go/internal/protocol"
	"github.com/guygrigsby/agent-go/internal/snapshot"
)

// requestLog appends one JSONL record per handled request: op, outcome,
// rejection evidence, latency, and a request hash for exact-resend
// detection. It is the raw material for per-episode counters (op mix,
// resubmissions, repair uptake, time to first accepted mutation). A nil
// *requestLog disables logging; note is nil-safe.
type requestLog struct {
	mu sync.Mutex
	f  *os.File
}

func openRequestLog(path string) (*requestLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &requestLog{f: f}, nil
}

func (l *requestLog) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}

func (l *requestLog) note(req protocol.Request, res map[string]any, start time.Time) {
	if l == nil {
		return
	}
	rec := map[string]any{
		"ts":      start.UTC().Format(time.RFC3339Nano),
		"op":      req.Op,
		"ms":      time.Since(start).Milliseconds(),
		"req_sha": reqSHA(req),
	}
	if req.Pkg != "" {
		rec["pkg"] = req.Pkg
	}
	if req.Sym != "" {
		rec["sym"] = req.Sym
	}
	if req.Generation != 0 {
		rec["generation"] = req.Generation
	}
	outcome, _ := res["status"].(string)
	rec["outcome"] = outcome
	if outcome == "rejected" {
		rec["reason"] = res["reason"]
		repairs := 0
		if reps, ok := res["possible_repairs"].([]snapshot.Repair); ok {
			repairs = len(reps)
		}
		rec["repairs"] = repairs
	}
	b, _ := json.Marshal(rec)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}
