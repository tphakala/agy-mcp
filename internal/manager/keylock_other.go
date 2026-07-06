//go:build !linux && !windows

package manager

import "github.com/tphakala/agy-mcp/internal/proc"

// flockExclusiveNB has no implementation on platforms without supervision (e.g.
// macOS); Linux uses flock and Windows uses LockFileEx (keylock_windows.go).
// StartJob refuses to spawn on unsupported platforms (proc.Supported) before any
// admission, and restore never finds a live job there (processAlive is a stub),
// so this is never reached for a real run; it exists only so the package compiles.
func flockExclusiveNB(_ uintptr) (bool, error) { return false, proc.ErrUnsupported }
