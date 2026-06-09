package manager

import "sync"

// gate enforces a global concurrency cap and per-key (conversation/cwd)
// serialization so concurrent agy sessions cannot trigger the known
// session-lock hang.
type gate struct {
	mu       sync.Mutex
	max      int
	inFlight int
	keys     map[string]bool
}

func newGate(max int) *gate {
	if max <= 0 {
		max = 1
	}
	return &gate{max: max, keys: map[string]bool{}}
}

// tryAcquire reserves a slot. An empty key skips per-key serialization but still
// counts against the global cap.
func (g *gate) tryAcquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight >= g.max {
		return false
	}
	if key != "" && g.keys[key] {
		return false
	}
	g.inFlight++
	if key != "" {
		g.keys[key] = true
	}
	return true
}

func (g *gate) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	if key != "" {
		delete(g.keys, key)
	}
}

// keyFor returns the serialization key for a request, or "" for a fresh run
// (no conversation, no continue) which needs no per-key lock.
//
// Fresh runs return "" today because the snapshot-diff UUID capture in
// conversation.go is not wired into Status yet. If that lazy capture is ever
// enabled, fresh runs sharing a cwd MUST be serialized too (return
// "cwd:"+req.Cwd here), otherwise two new conversations created in the same
// directory could misattribute their UUIDs (design spec section 5.4).
func keyFor(req StartRequest) string {
	if req.ConversationID != "" {
		return "conv:" + req.ConversationID
	}
	if req.ContinueLatest && req.Cwd != "" {
		return "cwd:" + req.Cwd
	}
	return ""
}
