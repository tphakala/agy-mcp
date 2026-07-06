package manager

import (
	"os"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// requestCancel writes the cancel sentinel file the supervisor polls for. Windows
// has no POSIX signal to deliver to an arbitrary process, so the manager and
// supervisor communicate the cancel through the shared on-disk job dir instead.
// The supervisor only checks for the file's existence, so no content or atomic
// write is needed; the file persists until the job dir is removed, so a cancel
// requested before the supervisor starts polling is never lost.
func (m *Manager) requestCancel(meta jobstore.Meta) error {
	dir, err := m.store.Dir(meta.ID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(jobstore.CancelPath(dir), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
