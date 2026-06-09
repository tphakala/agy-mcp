//go:build !linux

package manager

import "github.com/tphakala/agy-mcp/internal/jobstore"

// Non-Linux liveness stubs. Real liveness reads the kernel boot id and /proc, which only
// exist on Linux. StartJob refuses to spawn on unsupported platforms, so these are never
// reached for a live job; they exist so the package compiles on macOS and Windows.

func readBootID() string { return "" }

func readStartTimeTicks(_ int) (uint64, bool) { return 0, false }

func (m *Manager) processAlive(_ jobstore.Meta) bool { return false }
