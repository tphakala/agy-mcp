//go:build !linux && !windows && !darwin

package proc

import (
	"os/exec"
	"syscall"
)

// Non-Linux, non-Windows stubs so the manager and supervisor packages build on
// platforms (e.g. macOS) without a supervision implementation. Both callers
// check Supported and refuse before spawning, so these are never reached at
// runtime.

// Supported is false here: supervision relies on process groups / job objects.
const Supported = false

func ConfigureGroup(_ *exec.Cmd) {}

func StartDetached(_ *exec.Cmd) error { return ErrUnsupported }

// Group has no state on unsupported platforms; Track never returns one.
type Group struct{}

func Track(_ *exec.Cmd, _ bool) (*Group, error) { return nil, ErrUnsupported }

func (g *Group) Terminate(_ syscall.Signal) error { return ErrUnsupported }

func (g *Group) Close() error { return nil }
