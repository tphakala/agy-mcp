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
	if g.tryAcquire("conv:x") != acquireOK {
		t.Fatal("first acquire should succeed")
	}
	if g.tryAcquire("conv:x") != acquireKeyBusy {
		t.Fatal("second acquire on same key must report acquireKeyBusy while first is held")
	}
	g.release("conv:x")
	if g.tryAcquire("conv:x") != acquireOK {
		t.Fatal("acquire after release should succeed")
	}
}

// TestNewGateClampsNonPositive: a non-positive cap is clamped to 1, so a
// misconfigured MaxConcurrency cannot disable the gate (which would let every
// run proceed concurrently and re-expose the session-lock hang).
func TestNewGateClampsNonPositive(t *testing.T) {
	for _, max := range []int{0, -1, -100} {
		g := newGate(max)
		if g.cap() != 1 {
			t.Fatalf("newGate(%d).cap() = %d, want 1", max, g.cap())
		}
		if g.tryAcquire("a") != acquireOK {
			t.Fatalf("newGate(%d): first acquire should succeed", max)
		}
		if g.tryAcquire("b") != acquireAtCap {
			t.Fatalf("newGate(%d): a second distinct key must hit the clamped cap of 1", max)
		}
	}
}

// TestGateReleaseUnderflow: releasing more than was acquired must not drive
// inFlight negative (which would silently raise the effective cap). An extra
// release is absorbed, and the cap accounting stays correct afterward.
func TestGateReleaseUnderflow(t *testing.T) {
	g := newGate(1)
	g.release("never-acquired") // underflow: inFlight is already 0
	g.release("")               // and again with an empty key

	// The cap is still 1: one acquire works, the next is refused.
	if g.tryAcquire("a") != acquireOK {
		t.Fatal("acquire after underflow releases should succeed")
	}
	if g.tryAcquire("b") != acquireAtCap {
		t.Fatal("cap must still be 1 after underflow releases (inFlight not negative)")
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
	// inFlight is now past the cap, so a new normal run is refused for cap reasons.
	if g.tryAcquire("cwd:d") != acquireAtCap {
		t.Fatal("a new run must be refused (acquireAtCap) while forced jobs exceed the cap")
	}
	// Drain below the cap; the still-held forced key cwd:c must keep blocking a
	// same-key run even though a slot is now free (key block, not cap block).
	g.release("cwd:a")
	g.release("cwd:b")
	if g.tryAcquire("cwd:c") != acquireKeyBusy {
		t.Fatal("a forced key must keep blocking a same-key run (acquireKeyBusy) with a free slot")
	}
	// A different key with a free slot is still acquirable.
	if g.tryAcquire("cwd:d") != acquireOK {
		t.Fatal("a free key under the cap should be acquirable")
	}
}

func TestGateGlobalCap(t *testing.T) {
	g := newGate(2)
	if g.tryAcquire("conv:a") != acquireOK || g.tryAcquire("conv:b") != acquireOK {
		t.Fatal("two distinct keys should fill the cap")
	}
	// The cap is full even though the third key is distinct.
	if g.tryAcquire("conv:c") != acquireAtCap {
		t.Fatal("acquire past the global cap must report acquireAtCap")
	}
	g.release("conv:a")
	if g.tryAcquire("conv:c") != acquireOK {
		t.Fatal("acquire after a release should succeed")
	}
}

// TestGateRejectionDistinguishesCause: tryAcquire must report whether a refusal
// was a per-key conflict or the global cap, so the caller can give a precise
// error instead of always blaming a conversation/directory conflict. When both
// apply (cap full and the key already held) the more specific key conflict wins.
func TestGateRejectionDistinguishesCause(t *testing.T) {
	g := newGate(1)
	if g.tryAcquire("conv:a") != acquireOK {
		t.Fatal("first acquire should succeed")
	}
	if got := g.tryAcquire("conv:a"); got != acquireKeyBusy {
		t.Fatalf("same-key acquire = %v, want acquireKeyBusy even when also at cap", got)
	}
	if got := g.tryAcquire("conv:b"); got != acquireAtCap {
		t.Fatalf("distinct-key acquire at cap = %v, want acquireAtCap", got)
	}
	if g.cap() != 1 {
		t.Fatalf("cap() = %d, want 1", g.cap())
	}
}
