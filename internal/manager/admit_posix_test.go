//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/internal/config"
)

// twoManagers returns two managers that share one state dir, modeling two sibling
// agy-mcp processes in stdio mode (each process has its own in-memory gate, but
// they share AGY_MCP_STATE_DIR and so must serialize through the on-disk locks).
func twoManagers(t *testing.T) (m1, m2 *Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, MaxConcurrency: 4}
	return New(cfg), New(cfg)
}

// TestAdmitExcludesAcrossManagers: a fresh-run key admitted by one manager must be
// refused to a sibling manager sharing the state dir, then admittable once released.
// This is the cross-process serialization the in-process gate alone cannot provide.
func TestAdmitExcludesAcrossManagers(t *testing.T) {
	m1, m2 := twoManagers(t)

	if outcome, err := m1.admit("cwd:/w"); err != nil || outcome != acquireOK {
		t.Fatalf("m1.admit = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
	// m2's own gate is free, so only the cross-process lock can refuse it.
	if outcome, err := m2.admit("cwd:/w"); err != nil || outcome != acquireKeyBusy {
		t.Fatalf("m2.admit while m1 holds = (%v, %v), want (acquireKeyBusy, nil)", outcome, err)
	}
	// A distinct key is independent.
	if outcome, err := m2.admit("cwd:/other"); err != nil || outcome != acquireOK {
		t.Fatalf("m2.admit distinct key = (%v, %v), want (acquireOK, nil)", outcome, err)
	}

	m1.releaseKey("cwd:/w")
	if outcome, err := m2.admit("cwd:/w"); err != nil || outcome != acquireOK {
		t.Fatalf("m2.admit after m1.releaseKey = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
}

// TestAdmitEmptyKeyNotSerialized: an empty key has nothing to serialize on, so
// admit must skip the cross-process lock entirely and never block a sibling. This
// pins the key == "" short-circuit in admit/releaseKey that StartJob relies on for
// the defensive no-cwd path.
func TestAdmitEmptyKeyNotSerialized(t *testing.T) {
	m1, m2 := twoManagers(t)

	if outcome, err := m1.admit(""); err != nil || outcome != acquireOK {
		t.Fatalf("m1.admit(\"\") = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
	// A sibling admitting an empty key is not blocked: empty keys do not serialize.
	if outcome, err := m2.admit(""); err != nil || outcome != acquireOK {
		t.Fatalf("m2.admit(\"\") while m1 holds an empty key = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
	// No cross-process lock fd was taken for the empty key in either process.
	if len(m1.xlock.fds) != 0 || len(m2.xlock.fds) != 0 {
		t.Fatalf("empty key must take no cross-process lock; fds = %d, %d", len(m1.xlock.fds), len(m2.xlock.fds))
	}
	// releaseKey on an empty key is a no-op on the lock side and must not panic.
	m1.releaseKey("")
	m2.releaseKey("")
}

// TestAdmitInProcessRefusalsStillApply: admit must preserve the in-process gate's
// own outcomes (same-key busy, at-cap) and not let the cross-process layer mask
// them, so the precise StartJob error is unchanged.
func TestAdmitInProcessRefusalsStillApply(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 1})

	if outcome, err := m.admit("conv:a"); err != nil || outcome != acquireOK {
		t.Fatalf("first admit = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
	// Same key, same process: the in-process gate reports key-busy.
	if outcome, err := m.admit("conv:a"); err != nil || outcome != acquireKeyBusy {
		t.Fatalf("same-key admit = (%v, %v), want (acquireKeyBusy, nil)", outcome, err)
	}
	// Distinct key at the cap of 1: the in-process gate reports at-cap.
	if outcome, err := m.admit("conv:b"); err != nil || outcome != acquireAtCap {
		t.Fatalf("at-cap admit = (%v, %v), want (acquireAtCap, nil)", outcome, err)
	}
}

// TestAdmitFailsClosedOnLockError: when the cross-process lock cannot be
// established (the locks dir is uncreatable because the state dir is a regular
// file), admit returns an error and rolls back the in-process reservation, so the
// key is not left held. StartJob turns the error into a refusal.
func TestAdmitFailsClosedOnLockError(t *testing.T) {
	base := t.TempDir()
	fileAsStateDir := filepath.Join(base, "file")
	if err := os.WriteFile(fileAsStateDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	m := New(config.Config{StateDir: fileAsStateDir, MaxConcurrency: 4})

	if _, err := m.admit("cwd:/w"); err == nil {
		t.Fatal("admit must fail closed when the cross-process lock cannot be created")
	}
	// The in-process reservation was rolled back: the gate slot/key is free, so a
	// later admit (once the lock dir is fixed) is not wrongly blocked in-process.
	if m.gate.keys["cwd:/w"] {
		t.Fatal("admit must roll back the in-process key on a lock error")
	}
	// Rollback must also release the slot, not just the key: a leaked in-flight slot
	// would silently lower the effective cap while the key map looks clean.
	if m.gate.inFlight != 0 {
		t.Fatalf("admit must roll back the in-flight count on a lock error; got %d", m.gate.inFlight)
	}
}

// TestForceAdmitTakesCrossProcessLock: a restored job (whose detached supervisor
// outlived a manager restart) must re-take the cross-process lock so a sibling is
// blocked, and releaseKey must drop it.
func TestForceAdmitTakesCrossProcessLock(t *testing.T) {
	m1, m2 := twoManagers(t)

	if !m1.forceAdmit("cwd:/w") {
		t.Fatal("forceAdmit should track a restored job")
	}
	if outcome, err := m2.admit("cwd:/w"); err != nil || outcome != acquireKeyBusy {
		t.Fatalf("m2.admit while m1 force-holds = (%v, %v), want (acquireKeyBusy, nil)", outcome, err)
	}
	m1.releaseKey("cwd:/w")
	if outcome, err := m2.admit("cwd:/w"); err != nil || outcome != acquireOK {
		t.Fatalf("m2.admit after m1.releaseKey = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
}

// TestForceAdmitTracksDespiteSiblingLock: when a sibling already holds the
// cross-process lock (it grabbed the key during the manager-down restart gap),
// forceAdmit must still track the restored job in-process (it is genuinely
// running), and the later releaseKey must not release the sibling's lock.
func TestForceAdmitTracksDespiteSiblingLock(t *testing.T) {
	m1, m2 := twoManagers(t)

	// m2 (the sibling) grabbed the key during the gap.
	if outcome, err := m2.admit("cwd:/w"); err != nil || outcome != acquireOK {
		t.Fatalf("m2.admit = (%v, %v), want (acquireOK, nil)", outcome, err)
	}
	// m1 restores a still-running job for the same key: tracked in-process even
	// though it could not take the (sibling-held) cross-process lock.
	if !m1.forceAdmit("cwd:/w") {
		t.Fatal("forceAdmit must track a restored job even when a sibling holds the lock")
	}
	// m1 never held the lock, so its release must not free the sibling's lock.
	m1.releaseKey("cwd:/w")
	m3 := New(config.Config{StateDir: m2.cfg.StateDir, MaxConcurrency: 4})
	if outcome, err := m3.admit("cwd:/w"); err != nil || outcome != acquireKeyBusy {
		t.Fatalf("m3.admit while m2 still holds = (%v, %v), want (acquireKeyBusy, nil)", outcome, err)
	}
}
