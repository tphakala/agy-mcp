package manager

import (
	"fmt"
)

// Cancel asks the job's supervisor to terminate its agy child. The supervisor
// then terminates agy and writes the exit-code sentinel, so the job ends in a
// definite "cancelled" state. If the supervisor is no longer alive (already
// exited, or a stale PID), Cancel is a no-op success and Status reports the
// terminal state from disk.
//
// How the request reaches the supervisor is platform-specific (requestCancel):
// Linux forwards SIGTERM to the supervisor PID; Windows writes a cancel sentinel
// file the supervisor polls for, since Windows has no POSIX signal to deliver to
// an arbitrary process.
func (m *Manager) Cancel(id string) error {
	meta, err := m.store.Load(id)
	if err != nil {
		return fmt.Errorf("load job %s to cancel: %w", id, err)
	}
	// Confirm the recorded PID is still our supervisor before acting, so a recycled
	// PID is never cancelled. requestCancel tolerates a supervisor that exits
	// between this check and the request.
	if !m.processAlive(meta) {
		return nil
	}
	if err := m.requestCancel(meta); err != nil {
		return fmt.Errorf("request supervisor cancel: %w", err)
	}
	return nil
}
