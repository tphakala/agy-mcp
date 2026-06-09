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
