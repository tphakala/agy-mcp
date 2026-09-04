//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"testing"
)

// twoManagers returns two managers that share one state dir, modeling two sibling
// agy-mcp processes in stdio mode (each process has its own in-memory gate, but
// they share AGY_MCP_STATE_DIR and so must serialize through the on-disk locks).
func twoManagers(t *testing.T) (m1, m2 *Manager) {
	t.Helper()
	dir := t.TempDir()
	opts := managerOpts{stateDir: dir, maxConcurrency: 4}
	return newManager(t, opts), newManager(t, opts)
}

// TestAdmitExcludesAcrossManagers: a fresh-run key admitted by one manager must be
// refused to a sibling manager sharing the state dir, then admittable once released.
// This is the cross-process serialization the in-process gate alone cannot provide.
func TestIdempotencyClaimExcludesAcrossManagers(t *testing.T) {
	state := t.TempDir()
	m1 := newManager(t, managerOpts{stateDir: state, maxConcurrency: 4})
	m2 := newManager(t, managerOpts{stateDir: state, maxConcurrency: 4})

	ok, err := m1.acquireIdempotency("retry-1")
	if err != nil || !ok {
		t.Fatalf("first acquireIdempotency = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := m2.acquireIdempotency("retry-1"); err != nil || ok {
		t.Fatalf("sibling acquireIdempotency = (%v, %v), want (false, nil)", ok, err)
	}
	m1.releaseIdempotency("retry-1")
	if ok, err := m2.acquireIdempotency("retry-1"); err != nil || !ok {
		t.Fatalf("acquire after release = (%v, %v), want (true, nil)", ok, err)
	}
	m2.releaseIdempotency("retry-1")
}

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
	m := newManager(t, managerOpts{maxConcurrency: 1})

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
	m := newManager(t, managerOpts{stateDir: fileAsStateDir, maxConcurrency: 4})

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
	m3 := newManager(t, managerOpts{stateDir: m2.cfg.StateDir, maxConcurrency: 4})
	if outcome, err := m3.admit("cwd:/w"); err != nil || outcome != acquireKeyBusy {
		t.Fatalf("m3.admit while m2 still holds = (%v, %v), want (acquireKeyBusy, nil)", outcome, err)
	}
}

// TestReleaseKeyReleasesCrossLockBeforeGate pins the release-ordering invariant from
// issue #81: releaseKey must drop the cross-process lock BEFORE the in-process gate
// slot, mirroring admit's acquire order (gate first, then xlock). If it releases in
// the same order it acquires, the intermediate state "gate key free but this
// process's own xlock fd still held" becomes observable: a concurrent same-key admit
// passes gate.tryAcquire, then xlock.tryLock hits its own duplicate guard and returns
// the self-inflicted error `crosslock: key %q already held by this process` instead
// of a clean acquireKeyBusy refusal.
//
// The testHookMidRelease seam fires synchronously between releaseKey's two unlock
// steps, so the concurrent admit is interposed at exactly the intermediate point with
// no timing dependence (no goroutines, no sleeps). On the correct order the interposed
// admit sees the gate key still held and returns (acquireKeyBusy, nil); on the buggy
// order it sees the gate free but the lock held and returns (acquireOK, non-nil error).
func TestReleaseKeyReleasesCrossLockBeforeGate(t *testing.T) {
	m := newManager(t, managerOpts{maxConcurrency: 4})
	const key = "cwd:/w"

	if outcome, err := m.admit(key); err != nil || outcome != acquireOK {
		t.Fatalf("admit = (%v, %v), want (acquireOK, nil)", outcome, err)
	}

	var midOutcome acquireOutcome
	var midErr error
	m.testHookMidRelease = func() {
		m.testHookMidRelease = nil // fire once; the interposed admit must not re-enter
		midOutcome, midErr = m.admit(key)
	}

	m.releaseKey(key)

	if midErr != nil {
		t.Fatalf("concurrent same-key admit interposed during releaseKey returned error %v; "+
			"releaseKey must free the cross-process lock before the gate slot so the "+
			"intermediate state is never observable", midErr)
	}
	if midOutcome != acquireKeyBusy {
		t.Fatalf("concurrent same-key admit interposed during releaseKey = %v, want acquireKeyBusy "+
			"(the gate key must stay held until the cross-process lock is released)", midOutcome)
	}
}
