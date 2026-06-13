package manager

import (
	"log"
	"sync"
)

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

// acquireOutcome reports whether tryAcquire reserved a slot and, when it did
// not, why. The caller turns the cause into a precise error instead of always
// blaming a conversation/directory conflict (a refusal can also be the global
// cap, which is a different thing to tell the user).
type acquireOutcome int

const (
	acquireOK      acquireOutcome = iota // slot reserved
	acquireKeyBusy                       // another job already holds this conversation/cwd key
	acquireAtCap                         // the global concurrency cap is reached
)

// tryAcquire reserves a slot. An empty key skips per-key serialization but still
// counts against the global cap. The per-key check comes first so that when both
// causes apply (the cap is full and this key is already held) the more specific
// key conflict is reported.
func (g *gate) tryAcquire(key string) acquireOutcome {
	g.mu.Lock()
	defer g.mu.Unlock()
	if key != "" && g.keys[key] {
		return acquireKeyBusy
	}
	if g.inFlight >= g.max {
		return acquireAtCap
	}
	g.inFlight++
	if key != "" {
		g.keys[key] = true
	}
	return acquireOK
}

// cap returns the configured global concurrency cap, so callers can name the
// limit in an at-capacity error. It is set once in newGate and never mutated.
func (g *gate) cap() int { return g.max }

// forceAcquire reserves a slot and key for a job that is already running (a
// restored job whose agy session exists regardless of the gate), so the gate
// accounts for it. Unlike tryAcquire it ignores the cap: a live job must be
// counted even when live jobs exceed max (for example the cap was lowered across a
// restart), which then correctly blocks new runs until enough restored jobs drain.
// It still returns false without double-counting when the key is already held, so
// the caller starts exactly one liveness watcher per key.
func (g *gate) forceAcquire(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
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
	} else {
		// A release without a matching acquire means a gate-lifecycle bug (a double
		// release, or a release on a key that was never acquired). The clamp keeps
		// inFlight from going negative and silently raising the cap; log it so the
		// regression surfaces instead of hiding.
		log.Printf("gate release underflow for key %q (released more than acquired)", key)
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
