//go:build !linux && !windows && !darwin

package manager

import "github.com/tphakala/agy-mcp/v2/internal/proc"

// flockExclusiveNB has no implementation on platforms without supervision (e.g.
// FreeBSD); Linux and macOS use flock (keylock_posix.go) and Windows uses
// LockFileEx (keylock_windows.go). StartJob refuses to spawn on unsupported
// platforms (proc.Supported) before any admission, and restore never finds a
// live job there (processAlive is a stub), so this is never reached for a real
// run; it exists only so the package compiles.
func flockExclusiveNB(_ uintptr) (bool, error) { return false, proc.ErrUnsupported }
