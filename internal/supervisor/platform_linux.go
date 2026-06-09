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

// terminateGroup sends sig to the entire process group led by pid.
func terminateGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
