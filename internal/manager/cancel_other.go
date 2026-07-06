//go:build !linux && !windows

package manager

import "github.com/tphakala/agy-mcp/internal/jobstore"

// requestCancel is never reached on unsupported platforms: proc.Supported is
// false there, so no job is ever started and processAlive always reports false,
// making Cancel return before it calls this. The stub exists so the package
// compiles.
func (m *Manager) requestCancel(_ jobstore.Meta) error { return nil }
