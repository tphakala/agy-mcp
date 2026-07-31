package manager

import (
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// requestCancel writes the cancel sentinel file the supervisor polls for. Windows
// has no POSIX signal to deliver to an arbitrary process, so the manager and
// supervisor communicate the cancel through the shared on-disk job dir instead.
// The supervisor only checks for the file's existence, so no content is needed;
// the file persists until the job dir is removed, so a cancel requested before
// the supervisor starts polling is never lost.
//
// jobstore owns the write, as it does for every job-dir file it defines a
// writer for. That keeps the sentinel on the one atomic-write path (temp file
// plus rename, so a future content-reading consumer can never see a half-written
// file) rather than hand-rolling a second one here.
func (m *Manager) requestCancel(meta jobstore.Meta) error {
	dir, err := m.store.Dir(meta.ID)
	if err != nil {
		return err
	}
	return jobstore.WriteCancelDir(dir)
}
