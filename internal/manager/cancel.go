package manager

import (
	"fmt"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/proc"
)

// Cancel asks the job's supervisor to terminate its agy child. The supervisor
// forwards SIGTERM to agy and writes the exit-code sentinel, so the job ends in
// a definite "cancelled" state. If the supervisor is no longer alive (already
// exited, or a stale PID from a previous boot), Cancel is a no-op success and
// Status reports the terminal state from disk.
func (m *Manager) Cancel(id string) error {
	meta, err := m.store.Load(id)
	if err != nil {
		return fmt.Errorf("load job %s to cancel: %w", id, err)
	}
	// Confirm the recorded PID is still our supervisor (boot id plus /proc comm)
	// before signaling, so a recycled PID is never sent SIGTERM. proc.Signal treats
	// an already-exited pid (ESRCH) as success.
	if !m.processAlive(meta) {
		return nil
	}
	if err := proc.Signal(meta.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal supervisor: %w", err)
	}
	return nil
}
