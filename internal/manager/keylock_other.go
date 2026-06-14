//go:build !linux

package manager

import "github.com/tphakala/agy-mcp/internal/proc"

// flockExclusiveNB is unavailable off Linux. StartJob refuses to spawn on
// unsupported platforms (proc.Supported) before any admission, and restore never
// finds a live job there (processAlive is a stub), so this is never reached for a
// real run; it exists only so the package compiles on macOS and Windows.
func flockExclusiveNB(_ uintptr) (bool, error) { return false, proc.ErrUnsupported }
