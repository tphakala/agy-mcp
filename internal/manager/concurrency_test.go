package manager

import "testing"

func TestKeyForRequest(t *testing.T) {
	if k := keyFor(StartRequest{ConversationID: "abc"}); k != "conv:abc" {
		t.Errorf("conversation key = %q", k)
	}
	if k := keyFor(StartRequest{ContinueLatest: true, Cwd: "/w"}); k != "cwd:/w" {
		t.Errorf("continue-latest key = %q", k)
	}
	// A fresh run now serializes on its cwd, so two new conversations created in the
	// same directory cannot interleave and misattribute their captured UUIDs.
	if k := keyFor(StartRequest{Cwd: "/w"}); k != "cwd:/w" {
		t.Errorf("fresh run should serialize on cwd, got %q", k)
	}
	// With no cwd at all there is nothing to serialize on.
	if k := keyFor(StartRequest{}); k != "" {
		t.Errorf("keyless request = %q", k)
	}
}

func TestGateBlocksSameKey(t *testing.T) {
	g := newGate(4)
	if !g.tryAcquire("conv:x") {
		t.Fatal("first acquire should succeed")
	}
	if g.tryAcquire("conv:x") {
		t.Fatal("second acquire on same key must fail while first is held")
	}
	g.release("conv:x")
	if !g.tryAcquire("conv:x") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestGateForceAcquire(t *testing.T) {
	g := newGate(2)
	// A restored job is already running, so forceAcquire tracks it even past the cap.
	g.forceAcquire("cwd:a")
	g.forceAcquire("cwd:b")
	if !g.forceAcquire("cwd:c") {
		t.Fatal("forceAcquire must ignore the cap for an already-running job")
	}
	// A duplicate key is not double-tracked.
	if g.forceAcquire("cwd:c") {
		t.Fatal("forceAcquire on a held key must return false")
	}
	// inFlight is now past the cap, so a new normal run is refused.
	if g.tryAcquire("cwd:d") {
		t.Fatal("a new run must be refused while forced jobs exceed the cap")
	}
	// Drain below the cap; the still-held forced key cwd:c must keep blocking a
	// same-key run even though a slot is now free (key block, not cap block).
	g.release("cwd:a")
	g.release("cwd:b")
	if g.tryAcquire("cwd:c") {
		t.Fatal("a forced key must keep blocking a same-key run with a free slot")
	}
	// A different key with a free slot is still acquirable.
	if !g.tryAcquire("cwd:d") {
		t.Fatal("a free key under the cap should be acquirable")
	}
}

func TestGateGlobalCap(t *testing.T) {
	g := newGate(2)
	if !g.tryAcquire("conv:a") || !g.tryAcquire("conv:b") {
		t.Fatal("two distinct keys should fill the cap")
	}
	// The cap is full even though the third key is distinct.
	if g.tryAcquire("conv:c") {
		t.Fatal("acquire past the global cap must fail")
	}
	g.release("conv:a")
	if !g.tryAcquire("conv:c") {
		t.Fatal("acquire after a release should succeed")
	}
}
