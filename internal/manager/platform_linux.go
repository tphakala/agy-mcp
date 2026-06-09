//go:build linux

package manager

import (
	"errors"
	"os/exec"
	"syscall"
)

// platformSupported reports whether job supervision (spawning and signaling the agy
// process group, /proc liveness) runs on this OS. Only Linux is supported today; other
// platforms get the stubs in platform_other.go and StartJob refuses early with
// errPlatformUnsupported.
const platformSupported = true

// errPlatformUnsupported is what StartJob returns on an unsupported platform. It is nil
// on Linux; the symbol exists on every platform so the shared StartJob guard compiles.
var errPlatformUnsupported error

// setProcessGroup puts the spawned supervisor in its own process group, so the whole
// group (agy and any children) can be signaled together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup sends SIGTERM to the entire process group led by pid. It takes no
// signal parameter (the manager only ever needs SIGTERM) so manager.go stays free of
// the syscall import; the supervisor's own terminateGroup takes a signal because it
// also sends SIGKILL.
//
// A non-positive pid is rejected: syscall.Kill(-pid, ...) with pid <= 0 would target
// the caller's own process group and SIGTERM the manager. Current callers always pass a
// live cmd.Process.Pid (> 0); this guards a future caller or a corrupted meta.
func terminateGroup(pid int) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// signalProcess sends SIGTERM to a single supervisor pid. A pid that has already exited
// (ESRCH) is treated as success: there is nothing left to cancel. A non-positive pid is
// rejected so syscall.Kill never signals the caller's own process group.
func signalProcess(pid int) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
