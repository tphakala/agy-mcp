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

func newGate(maxJobs int) *gate {
	if maxJobs <= 0 {
		maxJobs = 1
	}
	return &gate{max: maxJobs, keys: map[string]bool{}}
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

// keyFor returns the serialization key for a request.
//
// A run with a resolved conversation id serializes on that conversation. Any
// other run with a cwd (a fresh run, or a continue_latest that found no prior
// conversation) serializes on the cwd: agy creates a new conversation for it,
// and the snapshot-diff UUID capture must hold the cwd key while it reads the
// shared conversation cache, otherwise two new conversations created in the same
// directory could misattribute their captured UUIDs.
//
// StartJob always populates req.Cwd, so the "" branch is effectively a defensive
// default; two distinct conversations in the same cwd still get distinct conv
// keys and run concurrently.
func keyFor(req StartRequest) string {
	if req.ConversationID != "" {
		return "conv:" + req.ConversationID
	}
	if req.Cwd != "" {
		return "cwd:" + req.Cwd
	}
	return ""
}
