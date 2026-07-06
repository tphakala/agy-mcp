//go:build linux

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

func TestConfigureGroupRequestsNewProcessGroup(t *testing.T) {
	cmd := exec.Command("true")
	ConfigureGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("ConfigureGroup must request a new process group (Setpgid)")
	}
}

// TestConfigureGroupPreservesExistingAttrs: ConfigureGroup must set only Setpgid,
// leaving any SysProcAttr fields a caller configured first intact.
func TestConfigureGroupPreservesExistingAttrs(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ConfigureGroup(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Error("ConfigureGroup must set Setpgid")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("ConfigureGroup must preserve a pre-existing SysProcAttr field (Setsid)")
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
// caller's own process group, so terminating a group led by a non-positive pid
// would kill the manager/supervisor itself. Group.Terminate and Signal must
// reject it.
func TestNonPositivePidRejected(t *testing.T) {
	for _, pid := range []int{0, -1, -1000} {
		g := &Group{pid: pid}
		if err := g.Terminate(syscall.SIGTERM); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("Group{pid:%d}.Terminate = %v, want EINVAL", pid, err)
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

// TestTrackRequiresStart: Track before Start has no pid to capture and must error
// rather than return a Group that would later terminate pid 0 (the caller's group).
func TestTrackRequiresStart(t *testing.T) {
	if _, err := Track(exec.Command("true"), false); err == nil {
		t.Fatal("Track before Start must return an error")
	}
}

// TestTrackTerminateKillsGroup: a Group captured after Start terminates the whole
// process group. The child sleeps; Terminate(SIGKILL) must end it.
func TestTrackTerminateKillsGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := Track(cmd, false)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := g.Terminate(syscall.SIGKILL); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the killed process to exit non-nil")
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
