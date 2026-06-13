//go:build linux

// Package proc holds the process-group primitives shared by the manager (which
// spawns and signals the supervisor) and the supervisor (which spawns and
// signals agy). They are meaningful only on Linux; other platforms get the
// no-op/error stubs in proc_other.go so both callers build everywhere and
// refuse early via Supported.
package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Supported reports whether process-group supervision runs on this OS. Callers
// check it and refuse before spawning on platforms where the stubs apply.
const Supported = true

// ErrUnsupported is what the off-Linux stubs return. It is nil on Linux; the
// symbol exists on every platform so a caller's shared guard compiles.
var ErrUnsupported error

// SetGroup puts the spawned process in its own process group, so the whole
// group (the child and its descendants) can be signaled together.
func SetGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// TerminateGroup sends sig to the entire process group led by pid. A
// non-positive pid is rejected: syscall.Kill(-pid, ...) with pid <= 0 would
// target the caller's own process group. Callers always pass a live
// cmd.Process.Pid (> 0); this guards a future caller or a corrupted meta.
func TerminateGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}

// Signal sends sig to a single pid (not its group). A pid that has already
// exited (ESRCH) is treated as success: there is nothing left to signal. A
// non-positive pid is rejected so it never signals the caller's own process
// group.
func Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
