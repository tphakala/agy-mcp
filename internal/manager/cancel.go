package manager

import (
	"fmt"
	"syscall"
)

// Cancel asks the job's supervisor to terminate its agy child. The supervisor
// forwards SIGTERM to agy and writes the exit-code sentinel, so the job ends in
// a definite "cancelled" state. If the supervisor is already gone, Cancel is a
// no-op success (Status will report the terminal state from disk).
func (m *Manager) Cancel(id string) error {
	meta, err := m.store.Load(id)
	if err != nil {
		return err
	}
	if meta.PID <= 0 {
		return nil
	}
	if err := syscall.Kill(meta.PID, syscall.SIGTERM); err != nil {
		// ESRCH means the supervisor already exited; treat as success.
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("signal supervisor: %w", err)
	}
	return nil
}
