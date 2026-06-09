//go:build !linux

package supervisor

import (
	"os/exec"
	"syscall"
)

// Non-Linux stubs. The supervisor is spawned only by the manager's StartJob, which
// refuses on non-Linux platforms, so Run is never invoked there; these exist so the
// package builds on macOS and Windows.

func setProcessGroup(_ *exec.Cmd) {}

func terminateGroup(_ int, _ syscall.Signal) error { return nil }
