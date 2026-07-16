// Package daemon serves snapshot operations over a per-workspace unix socket.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/guygrigsby/agent-go/internal/protocol"
	"github.com/guygrigsby/agent-go/internal/snapshot"
)

func SocketPath(dir string) string {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(cache, "ago", hex.EncodeToString(sum[:8])+".sock")
}

// Run serves until idle seconds pass with no requests. One request per
// connection; snapshot methods serialize internally. logPath, when
// non-empty, appends one JSONL record per request (see requestLog); the
// AGO_LOG_REQUESTS env var reaches auto-spawned daemons because spawn
// inherits the parent environment.
func Run(dir string, idle time.Duration, logPath string) error {
	var rlog *requestLog
	if logPath != "" {
		l, err := openRequestLog(logPath)
		if err != nil {
			return err
		}
		rlog = l
		defer rlog.Close()
	}
	sock := SocketPath(dir)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer l.Close()
	defer os.Remove(sock)

	snap := snapshot.New(dir)
	breaker := newResendBreaker()
	timer := time.AfterFunc(idle, func() { l.Close() })
	for {
		conn, err := l.Accept()
		if err != nil {
			return nil // idle close or shutdown
		}
		timer.Reset(idle)
		if stop := handleWithBreaker(conn, snap, rlog, breaker); stop {
			return nil
		}
	}
}

func handle(conn net.Conn, snap *snapshot.Snapshot, rlog *requestLog) (stop bool) {
	return handleWithBreaker(conn, snap, rlog, nil)
}

func handleWithBreaker(conn net.Conn, snap *snapshot.Snapshot, rlog *requestLog, breaker *resendBreaker) (stop bool) {
	defer conn.Close()
	var req protocol.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeJSON(conn, map[string]any{"status": "error", "error": "bad request: " + err.Error()})
		return false
	}
	start := time.Now()
	var res map[string]any
	var err error
	switch req.Op {
	case "status":
		res, err = snap.Status()
	case "help":
		res, err = snap.Help()
	case "inspect":
		res, err = snap.Inspect(req.Pkg, req.Sym)
	case "view":
		res, err = snap.View(req.Pkg, req.Sym)
	case "refs":
		res, err = snap.Refs(req.Pkg, req.Sym)
	case "search":
		res, err = snap.Search(req.Sym)
	case "query":
		// Sym doubles as q for kind=="search", the same reuse the standalone
		// search op already does — no separate wire field for it.
		res, err = snap.Query(req.Kind, req.Pkg, req.Sym, req.Sym)
	case "set-body":
		res, err = snap.SetBody(req.Pkg, req.Sym, req.Body)
	case "rename":
		res, err = snap.Rename(req.Pkg, req.Sym, req.To)
	case "upsert":
		res, err = snap.UpsertDecl(req.Pkg, req.Body)
	case "add-param":
		res, err = snap.AddParam(req.Pkg, req.Sym, req.Name, req.Type, req.Def)
	case "test":
		// Sym doubles as the -run filter — no separate wire field for it,
		// the same reuse "query" already does for its q parameter.
		res, err = snap.TestRun(req.Pkg, req.Sym)
	case "patch":
		raw, merr := json.Marshal(req)
		if merr != nil {
			err = merr
		} else {
			res, err = snap.Patch(raw)
		}
	case "stop":
		writeJSON(conn, map[string]any{"status": "stopping"})
		return true
	default:
		res = map[string]any{"status": "error", "error": "unknown op " + req.Op}
	}
	if rej, ok := err.(*snapshot.Reject); ok {
		res = map[string]any{"status": "rejected", "reason": rej.Reason,
			"detail": rej.Detail, "diagnostics": rej.Diagnostics, "did_you_mean": rej.DidYouMean,
			"possible_repairs": rej.PossibleRepairs}
	} else if err != nil {
		res = map[string]any{"status": "error", "error": err.Error()}
	}
	sha := reqSHA(req)
	if res["status"] == "rejected" {
		if n := breaker.bump(sha); n > 0 {
			res["resent"] = n
			res["escalation"] = "this exact call was already rejected; do not resend it unchanged — send a possible_repairs call verbatim, or change the arguments"
		}
	} else {
		breaker.clear(sha)
	}
	rlog.note(req, res, start)
	writeJSON(conn, res)
	return false
}

func writeJSON(conn net.Conn, v any) {
	enc := json.NewEncoder(conn)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
