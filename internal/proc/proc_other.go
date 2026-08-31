//go:build !linux && !windows && !darwin

package proc

import (
	"os/exec"
	"syscall"
)

// Non-Linux, non-Windows, non-macOS stubs so the manager and supervisor packages
// build on platforms (e.g. FreeBSD) without a supervision implementation. The
// spawning entry points (ConfigureGroup, StartDetached, Track) are gated behind
// Supported by their callers, which refuse before spawning, so those never run
// here. ConfigureNoWindow is the exception: the manager's probes call it
// unconditionally, so it does run, and does nothing.

// Supported is false here: supervision relies on process groups / job objects.
const Supported = false

func ConfigureGroup(_ *exec.Cmd) {}

func ConfigureNoWindow(_ *exec.Cmd) {}

func StartDetached(_ *exec.Cmd) error { return ErrUnsupported }

// Group has no state on unsupported platforms; Track never returns one.
type Group struct{}

func Track(_ *exec.Cmd, _ bool) (*Group, error) { return nil, ErrUnsupported }

func (g *Group) Terminate(_ syscall.Signal) error { return ErrUnsupported }

func (g *Group) Close() error { return nil }
