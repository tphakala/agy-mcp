//go:build !linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Non-Linux stubs so the manager and supervisor packages build on macOS and
// Windows. Both callers check Supported and refuse before spawning, so these
// are never reached at runtime.

// Supported is false off Linux: supervision relies on process groups and /proc.
const Supported = false

// ErrUnsupported is returned by the stubs and by callers that refuse early.
var ErrUnsupported = errors.New("agy-mcp: process supervision is only supported on Linux")

func SetGroup(_ *exec.Cmd) {}

func TerminateGroup(_ int, _ syscall.Signal) error { return ErrUnsupported }

func Signal(_ int, _ syscall.Signal) error { return ErrUnsupported }
