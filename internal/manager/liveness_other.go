//go:build !linux && !windows && !darwin

package manager

import "github.com/tphakala/agy-mcp/v2/internal/jobstore"

// Liveness stubs for platforms with no supervision implementation.
// Linux reads the kernel boot id and /proc; Windows queries the process via
// Win32 (liveness_windows.go); darwin reads sysctl kern.proc.pid
// (liveness_darwin.go). StartJob refuses to spawn on unsupported platforms,
// so these are never reached for a live job; they exist so the package compiles.

// startTimeMandatory reports whether StartJob must fail when it cannot record a
// supervisor start time. It is false wherever processAlive has a name-based
// liveness fallback (Linux /proc/comm, Windows image name) or never runs a live
// job (the unsupported stub); only darwin (no fallback) sets it true.
const startTimeMandatory = false

func readBootID() string { return "" }

func readStartTimeTicks(_ int) (uint64, bool) { return 0, false }

func (m *Manager) processAlive(_ jobstore.Meta) bool { return false }
