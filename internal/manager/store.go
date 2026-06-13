package manager

import "github.com/tphakala/agy-mcp/internal/jobstore"

// jobStore is the subset of *jobstore.Store that the Manager depends on. It is an
// interface (not the concrete type) so tests can inject a store whose methods fail
// on demand, matching the existing field-injection pattern (cacheFile, captureBudget).
// *jobstore.Store satisfies it structurally, so production code is unchanged.
//
// It is the full public surface of *jobstore.Store today; the goal is an injectable
// seam, not a minimized subset. If Manager starts calling a new store method, add it
// here too (a missing method makes *jobstore.Store stop satisfying the interface and
// fails the build, so the omission cannot pass silently).
type jobStore interface {
	Create(m jobstore.Meta) (string, error)
	Load(id string) (jobstore.Meta, error)
	UpdateMeta(m jobstore.Meta) error
	SetConversationID(id, convID string) (string, error)
	Remove(id string) error
	Dir(id string) (string, error)
	WriteExitCode(id string, code int) error
	ExitCode(id string) (int, bool)
	List() ([]string, error)
}
