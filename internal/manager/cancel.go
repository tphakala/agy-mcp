package manager

import (
	"errors"
	"fmt"
	"syscall"
)

// Cancel asks the job's supervisor to terminate its agy child. The supervisor
// forwards SIGTERM to agy and writes the exit-code sentinel, so the job ends in
// a definite "cancelled" state. If the supervisor is no longer alive (already
// exited, or a stale PID from a previous boot), Cancel is a no-op success and
// Status reports the terminal state from disk.
func (m *Manager) Cancel(id string) error {
	meta, err := m.store.Load(id)
	if err != nil {
		return err
	}
	// Confirm the recorded PID is still our supervisor (boot id plus /proc comm)
	// before signaling, so a recycled PID is never sent SIGTERM.
	if !m.processAlive(meta) {
		return nil
	}
	if err := syscall.Kill(meta.PID, syscall.SIGTERM); err != nil {
		// ESRCH means the supervisor exited between the liveness check and the
		// signal; treat it as success.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signal supervisor: %w", err)
	}
	return nil
}
