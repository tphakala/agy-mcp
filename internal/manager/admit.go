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

// releaseKey releases both the in-process gate slot/key and the cross-process lock
// for key. It pairs with admit and forceAdmit. The cross-process unlock is a no-op
// for a key this process does not hold (a restored job whose lock a sibling held),
// so it is always safe on the run's completion path.
func (m *Manager) releaseKey(key string) {
	m.gate.release(key)
	if key != "" {
		m.xlock.unlock(key)
	}
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
