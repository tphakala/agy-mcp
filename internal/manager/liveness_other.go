//go:build !linux && !windows

package manager

import "github.com/tphakala/agy-mcp/internal/jobstore"

// Liveness stubs for platforms with no supervision implementation (e.g. macOS).
// Linux reads the kernel boot id and /proc; Windows queries the process via
// Win32 (liveness_windows.go). StartJob refuses to spawn on unsupported platforms,
// so these are never reached for a live job; they exist so the package compiles.

func readBootID() string { return "" }

func readStartTimeTicks(_ int) (uint64, bool) { return 0, false }

func (m *Manager) processAlive(_ jobstore.Meta) bool { return false }
