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

// TestSetGroupPreservesExistingAttrs: SetGroup must set only Setpgid, leaving
// any SysProcAttr fields a caller configured first intact.
func TestSetGroupPreservesExistingAttrs(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	SetGroup(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Error("SetGroup must set Setpgid")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("SetGroup must preserve a pre-existing SysProcAttr field (Setsid)")
	}
}

// TestErrUnsupportedIsNonNil: ErrUnsupported must be a non-nil sentinel even on
// Linux, so a caller comparing a (nil) success error against it never gets a
// false match (errors.Is(nil, nil) is true).
func TestErrUnsupportedIsNonNil(t *testing.T) {
	if ErrUnsupported == nil {
		t.Fatal("ErrUnsupported must be a non-nil sentinel on every platform")
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
