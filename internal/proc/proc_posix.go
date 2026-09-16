//go:build linux || darwin

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Supported reports whether process-group supervision runs on this OS. Callers
// check it and refuse before spawning on platforms where the stubs apply.
const Supported = true

// ensureSysProcAttr allocates cmd.SysProcAttr when a caller has not set one, so the
// Configure* helpers can OR in their own field without clobbering attrs a caller
// configured first. It mirrors the helper of the same name in proc_windows.go.
func ensureSysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
}

// ConfigureGroup puts the spawned process in its own process group, so the whole
// group (the child and its descendants) can be terminated together via a Group
// captured by Track. The manager uses it (through StartDetached) for the detached
// run-job supervisor; the agy child itself uses ConfigureSession instead, which
// additionally drops the controlling terminal. It sets only Setpgid, preserving
// any other SysProcAttr fields a caller configured first.
func ConfigureGroup(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.Setpgid = true
}

// ConfigureSession puts the spawned process in a new session (Setsid), which gives
// it its own process group AND, crucially, no controlling terminal. The supervisor
// uses it for the agy child. agy opens /dev/tty and calls tcsetattr to put the
// terminal in raw mode at startup even under --print / --output-format stream-json;
// a process performing a terminal-modifying ioctl while in a background process
// group is stopped by SIGTTOU (state T) before it emits any output, which is why a
// run launched from a real terminal hangs until the print-timeout. Leading a new
// session removes the controlling terminal, so agy's open("/dev/tty") returns ENXIO
// and it skips the raw-mode setup instead of stopping.
//
// A session leader has pgid == pid, so the Track/Terminate group kill (kill -pgid)
// still tears the whole tree down; Setsid alone therefore replaces the Setpgid that
// ConfigureGroup sets. It preserves any other SysProcAttr fields a caller set first,
// but it must not be paired with Setpgid: setpgid on a session leader fails EPERM.
func ConfigureSession(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.Setsid = true
}

// ConfigureNoWindow is a no-op here. It exists so the manager's probe spawns can
// call it unconditionally; only Windows allocates a console window for a child
// that cannot inherit one.
func ConfigureNoWindow(_ *exec.Cmd) {}

// StartDetached configures cmd so the child leads its own process group and
// starts it, so the child (the detached supervisor) can be tracked as a group
// and survives the parent's death via init adoption. On Linux and macOS this is
// ConfigureGroup plus Start; the Windows build additionally sets the detach and
// job-breakaway creation flags that let the supervisor outlive the manager.
func StartDetached(cmd *exec.Cmd) error {
	ConfigureGroup(cmd)
	return cmd.Start()
}

// Group is a handle to a spawned process tree that can be terminated as a unit.
// On Linux and macOS it is identified by the leader pid (kill -pgid); the
// Windows build wraps a Job Object handle.
type Group struct {
	pid int
}

// Track captures a handle to the process group led by an already-started cmd, so
// the group can later be terminated together. cmd.Start must have succeeded.
// killOnClose is a Windows Job Object option (tear the tree down with the handle);
// it has no effect on Linux or macOS, where a crashing tracker orphans its
// process group rather than killing it, so the parameter is accepted for a
// common signature and ignored.
func Track(cmd *exec.Cmd, _ bool) (*Group, error) {
	if cmd.Process == nil {
		return nil, errors.New("proc: Track before Start")
	}
	return &Group{pid: cmd.Process.Pid}, nil
}

// Terminate sends sig to the entire process group. A non-positive leader pid is
// rejected: syscall.Kill(-pid, ...) with pid <= 0 would target the caller's own
// process group. Callers always pass a live pid captured at Start; this guards a
// future caller or a corrupted Group.
func (g *Group) Terminate(sig syscall.Signal) error {
	if g == nil || g.pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-g.pid, sig)
}

// Close releases the handle. On Linux and macOS there is nothing to release.
func (g *Group) Close() error { return nil }

// Signal sends sig to a single pid (not its group). A pid that has already
// exited (ESRCH) is treated as success: there is nothing left to signal. A
// non-positive pid is rejected so it never signals the caller's own process
// group. The manager's Linux and macOS cancel uses it to forward SIGTERM to the
// supervisor.
func Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
