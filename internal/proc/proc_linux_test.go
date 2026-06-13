//go:build linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

func TestSetGroupRequestsNewProcessGroup(t *testing.T) {
	cmd := exec.Command("true")
	SetGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("SetGroup must request a new process group (Setpgid)")
	}
}

// TestNonPositivePidRejected: syscall.Kill(-pid, ...) with pid <= 0 targets the
// caller's own process group, so signaling a non-positive pid would terminate
// the manager/supervisor itself. Both helpers must reject it.
func TestNonPositivePidRejected(t *testing.T) {
	for _, pid := range []int{0, -1, -1000} {
		if err := TerminateGroup(pid, syscall.SIGTERM); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("TerminateGroup(%d) = %v, want EINVAL", pid, err)
		}
		if err := Signal(pid, syscall.SIGTERM); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("Signal(%d) = %v, want EINVAL", pid, err)
		}
	}
}

// TestSignalToleratesAlreadyExited: an already-exited pid (ESRCH) is success;
// there is nothing left to cancel. Cancel relies on this so a supervisor that
// finished between the liveness check and the signal is not reported as a
// signal failure.
func TestSignalToleratesAlreadyExited(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap, so pid is gone and a later Kill sees ESRCH
	if err := Signal(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("Signal on an exited pid = %v, want nil (ESRCH tolerated)", err)
	}
}
