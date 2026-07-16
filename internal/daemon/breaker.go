package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/guygrigsby/agent-go/internal/protocol"
)

// reqSHA identifies a request by content; an identical resend hashes the
// same. Shared by the request log and the resend breaker.
func reqSHA(req protocol.Request) string {
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// resendBreaker escalates exact resends of just-rejected requests: a model
// that loops the same failing call gets a harder imperative instead of the
// same rejection until the episode cap. Nil disables it.
type resendBreaker struct {
	mu   sync.Mutex
	seen map[string]int
}

func newResendBreaker() *resendBreaker { return &resendBreaker{seen: map[string]int{}} }

// bump returns how many times this exact request was already rejected,
// then records this rejection.
func (b *resendBreaker) bump(sha string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.seen[sha]
	// ponytail: flat reset instead of LRU; 512 distinct rejected calls in
	// one daemon lifetime means the episode is lost anyway.
	if n == 0 && len(b.seen) >= 512 {
		b.seen = map[string]int{}
	}
	b.seen[sha] = n + 1
	return n
}

// clear forgets a request once it stops being rejected.
func (b *resendBreaker) clear(sha string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.seen, sha)
}
