//go:build linux || darwin

package testutil

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// KillProcessGroup must not return while a member of the group can still run
// (issue #196). The leader forks a grandchild and execs sleep, so the group has
// two members; a single SIGKILL pass returns before they have exited.
func TestKillProcessGroupLeavesNoLiveMember(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! ; exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	// Mirror the manager's reaper goroutine, which reaps the supervisor leader.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-reaped:
		case <-time.After(10 * time.Second):
			t.Error("leader was not reaped after cleanup SIGKILL")
		}
	})

	var grandchild int
	if _, err := fmt.Fscan(stdout, &grandchild); err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}

	KillProcessGroup(t, pgid, 10*time.Second)

	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("Kill(-%d, 0) = %v after KillProcessGroup, want ESRCH or EPERM: a member is still live", pgid, err)
	}
}
