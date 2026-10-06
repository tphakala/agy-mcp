//go:build darwin

package testutil

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// On macOS a group whose members are all unreaped zombies answers EPERM to
// kill(2) (MEASURED on macOS 26.7.1). KillProcessGroup must treat that as
// "nothing left that can run" and return instead of spinning to its timeout.
func TestKillProcessGroupReturnsWhenOnlyZombiesRemain(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Not reaped until cleanup, so after the SIGKILL the leader is a zombie.
	KillProcessGroup(t, cmd.Process.Pid, 2*time.Second)
}
