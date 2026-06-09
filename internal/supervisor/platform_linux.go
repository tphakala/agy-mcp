//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts agy in its own process group so the supervisor can signal the
// whole group (agy and its children) together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup sends sig to the entire process group led by pid. A non-positive pid is
// rejected: syscall.Kill(-pid, ...) with pid <= 0 would target the supervisor's own
// process group. Callers always pass a live cmd.Process.Pid (> 0); this guards against a
// future caller.
func terminateGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}
