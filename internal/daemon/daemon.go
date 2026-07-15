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
// connection; snapshot methods serialize internally.
func Run(dir string, idle time.Duration) error {
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
	timer := time.AfterFunc(idle, func() { l.Close() })
	for {
		conn, err := l.Accept()
		if err != nil {
			return nil // idle close or shutdown
		}
		timer.Reset(idle)
		if stop := handle(conn, snap); stop {
			return nil
		}
	}
}

func handle(conn net.Conn, snap *snapshot.Snapshot) (stop bool) {
	defer conn.Close()
	var req protocol.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeJSON(conn, map[string]any{"status": "error", "error": "bad request: " + err.Error()})
		return false
	}
	var res map[string]any
	var err error
	switch req.Op {
	case "status":
		res, err = snap.Status()
	case "inspect":
		res, err = snap.Inspect(req.Pkg, req.Sym)
	case "refs":
		res, err = snap.Refs(req.Pkg, req.Sym)
	case "search":
		res, err = snap.Search(req.Sym)
	case "set-body":
		res, err = snap.SetBody(req.Pkg, req.Sym, req.Body)
	case "rename":
		res, err = snap.Rename(req.Pkg, req.Sym, req.To)
	case "stop":
		writeJSON(conn, map[string]any{"status": "stopping"})
		return true
	default:
		res = map[string]any{"status": "error", "error": "unknown op " + req.Op}
	}
	if rej, ok := err.(*snapshot.Reject); ok {
		res = map[string]any{"status": "rejected", "reason": rej.Reason,
			"detail": rej.Detail, "diagnostics": rej.Diagnostics}
	} else if err != nil {
		res = map[string]any{"status": "error", "error": err.Error()}
	}
	writeJSON(conn, res)
	return false
}

func writeJSON(conn net.Conn, v any) {
	enc := json.NewEncoder(conn)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
