package manager

import (
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/proc"
)

// requestCancel forwards SIGTERM to the supervisor, which its signal handler turns
// into a graceful cancel of agy (SIGTERM, then SIGKILL after a grace window).
// proc.Signal treats an already-exited PID (ESRCH) as success.
func (m *Manager) requestCancel(meta jobstore.Meta) error {
	return proc.Signal(meta.PID, syscall.SIGTERM)
}
