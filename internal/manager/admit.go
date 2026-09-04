package manager

import "log"

// admit reserves both the in-process gate slot and the cross-process advisory lock
// for key, so the run is serialized against same-key runs in this process (via the
// gate) and in sibling processes that share the state dir (via the lock). It returns
// the gate's own outcome on an in-process refusal, acquireKeyBusy when a sibling
// process holds the key, and a non-nil error when the cross-process lock could not
// be established at all, which the caller must treat as fail-closed (refuse the run
// rather than proceed without exclusion). On every refusal and on error the
// in-process reservation is rolled back, so the caller releases nothing. The
// returned outcome is meaningful only when err is nil.
func (m *Manager) admit(key string) (acquireOutcome, error) {
	outcome := m.gate.tryAcquire(key)
	if outcome != acquireOK {
		return outcome, nil
	}
	// An empty key skips per-key serialization (there is nothing to serialize on),
	// so there is no cross-process lock to take; the global cap already counted it.
	if key == "" {
		return acquireOK, nil
	}
	locked, err := m.xlock.tryLock(key)
	if err != nil {
		m.gate.release(key) // reserved the slot but the run cannot safely proceed
		return acquireOK, err
	}
	if !locked {
		m.gate.release(key) // a sibling process holds the key
		return acquireKeyBusy, nil
	}
	return acquireOK, nil
}

// acquireIdempotency takes a short-lived creation claim for one retry token. It
// is independent of the conversation gate: a request can need both locks at once.
// The in-process map prevents crossLock's duplicate-held-key guard from surfacing
// as an internal error, while the hashed cross-process lock excludes sibling MCP
// server processes that share the state directory.
func (m *Manager) acquireIdempotency(key string) (bool, error) {
	lockKey := "idem:" + key
	m.idemMu.Lock()
	if m.idemKeys[lockKey] {
		m.idemMu.Unlock()
		return false, nil
	}
	m.idemKeys[lockKey] = true
	m.idemMu.Unlock()

	locked, err := m.xlock.tryLock(lockKey)
	if err != nil || !locked {
		m.idemMu.Lock()
		delete(m.idemKeys, lockKey)
		m.idemMu.Unlock()
	}
	return locked, err
}

func (m *Manager) releaseIdempotency(key string) {
	lockKey := "idem:" + key
	// Release the cross-process lock first, matching releaseKey's ordering: a
	// local waiter must not observe the key as free while this process still owns
	// the underlying flock.
	m.xlock.unlock(lockKey)
	m.idemMu.Lock()
	delete(m.idemKeys, lockKey)
	m.idemMu.Unlock()
}

// releaseKey releases both the in-process gate slot/key and the cross-process lock
// for key. It pairs with admit and forceAdmit. The cross-process unlock is a no-op
// for a key this process does not hold (a restored job whose lock a sibling held),
// so it is always safe on the run's completion path.
//
// Release order mirrors admit's acquire order in reverse: admit takes the gate slot
// first, then the cross-process lock, so releaseKey drops the cross-process lock
// first, then the gate slot. Releasing the gate slot LAST is what makes the fix for
// issue #81 correct rather than merely narrower: because admit checks the gate before
// ever reaching xlock.tryLock, holding the gate key until the lock is already gone
// means any concurrent same-key admit either bounces cleanly off the still-held gate
// (acquireKeyBusy) or, once it passes the gate, is guaranteed the lock is free. The
// gate.mu release-before-acquire edge orders this xlock.unlock ahead of that admit's
// later xlock.tryLock. Releasing in the other order exposed a transient window where
// the gate key was free but this process's own lock fd was still held, so a same-key
// admit hit xlock's duplicate guard and surfaced a self-inflicted "already held by
// this process" error instead of a clean refusal.
func (m *Manager) releaseKey(key string) {
	if key != "" {
		m.xlock.unlock(key)
	}
	if m.testHookMidRelease != nil {
		m.testHookMidRelease()
	}
	m.gate.release(key)
}

// forceAdmit tracks an already-running restored job (whose detached supervisor
// outlived a manager restart) in the in-process gate, then best-effort re-takes the
// cross-process lock so siblings are blocked again. Unlike admit it never refuses:
// the job is running and must be counted regardless of the cap. The cross-process
// lock is best effort because a sibling may have grabbed the key during the
// manager-down gap; when that happens the job is still tracked in-process and the
// lapse is logged. It returns false (without taking the lock) only when this process
// already tracks the key, so the caller starts exactly one watcher per key.
func (m *Manager) forceAdmit(key string) bool {
	if !m.gate.forceAcquire(key) {
		return false
	}
	if key == "" {
		return true
	}
	if locked, err := m.xlock.tryLock(key); err != nil {
		log.Printf("restore: cross-process lock for key %q failed: %v; job tracked in-process only", key, err)
	} else if !locked {
		log.Printf("restore: cross-process lock for key %q held by another process; job tracked in-process only", key)
	}
	return true
}
