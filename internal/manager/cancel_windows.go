package manager

import (
	"os"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
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
	// Create the sentinel atomically (temp file + rename), matching the rest of the
	// job-dir contract (meta.json, exit_code). The supervisor only checks the file's
	// existence, so an empty file is already a valid signal; the rename additionally
	// keeps any future content-reading consumer from ever seeing a half-written file.
	tmp, err := os.CreateTemp(dir, "cancel-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, jobstore.CancelPath(dir)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
