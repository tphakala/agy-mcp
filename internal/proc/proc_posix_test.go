//go:build linux || darwin

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
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
	// Noctty (not Setsid) is the sentinel: Setsid alongside the Setpgid ConfigureGroup
	// adds would model an un-startable combo (setpgid on a session leader is EPERM),
	// even though this test never starts the command.
	cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true}
	ConfigureGroup(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Error("ConfigureGroup must set Setpgid")
	}
	if !cmd.SysProcAttr.Noctty {
		t.Error("ConfigureGroup must preserve a pre-existing SysProcAttr field (Noctty)")
	}
}

// ConfigureNoWindow is a no-op off Windows, but the manager's probes call it
// unconditionally, so it must tolerate a command with no SysProcAttr and must
// not invent one or disturb an existing one.
func TestConfigureNoWindowIsInertHere(t *testing.T) {
	cmd := exec.Command("true")
	ConfigureNoWindow(cmd)
	if cmd.SysProcAttr != nil {
		t.Errorf("ConfigureNoWindow must not allocate SysProcAttr here, got %+v", cmd.SysProcAttr)
	}

	withAttrs := exec.Command("true")
	withAttrs.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	ConfigureNoWindow(withAttrs)
	if !withAttrs.SysProcAttr.Setsid {
		t.Error("ConfigureNoWindow must leave an existing SysProcAttr untouched")
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

// TestConfigureSessionRequestsNewSession: the agy child must run in its own
// session (Setsid), not merely its own process group. A new session has no
// controlling terminal, so agy's startup open("/dev/tty")+tcsetattr (raw mode),
// which it performs even under --output-format stream-json, finds no terminal
// (ENXIO) instead of raising SIGTTOU from a background process group, which
// would stop (state T) agy before it emits any output.
func TestConfigureSessionRequestsNewSession(t *testing.T) {
	cmd := exec.Command("true")
	ConfigureSession(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("ConfigureSession must request a new session (Setsid) so the child has no controlling terminal")
	}
}

// TestConfigureSessionPreservesExistingAttrs: ConfigureSession adds Setsid without
// clobbering a SysProcAttr field a caller configured first. The sentinel is Noctty
// (not Setpgid), since Setsid combined with Setpgid would try setpgid on a session
// leader and fail EPERM at spawn.
func TestConfigureSessionPreservesExistingAttrs(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true}
	ConfigureSession(cmd)
	if !cmd.SysProcAttr.Setsid {
		t.Error("ConfigureSession must set Setsid")
	}
	if !cmd.SysProcAttr.Noctty {
		t.Error("ConfigureSession must preserve a pre-existing SysProcAttr field (Noctty)")
	}
}

// TestConfigureSessionClearsConflictingGroupAttrs: ConfigureSession must neutralize a
// pre-existing process-group request so it is safe on top of another Configure call.
// Setsid makes the child a session (and group) leader; Go's fork path then runs
// setpgid, which fails EPERM on a session leader, so ConfigureGroup-then-ConfigureSession
// would otherwise die at Start. The cleared Setpgid must let the command start and run.
func TestConfigureSessionClearsConflictingGroupAttrs(t *testing.T) {
	cmd := exec.Command("true")
	ConfigureGroup(cmd)   // sets Setpgid = true
	ConfigureSession(cmd) // must clear Setpgid and set Setsid
	if cmd.SysProcAttr.Setpgid {
		t.Error("ConfigureSession must clear a pre-existing Setpgid (setpgid on a session leader fails EPERM)")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("ConfigureSession must set Setsid")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start after ConfigureGroup+ConfigureSession must succeed, got: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// TestConfigureSessionGroupStillTerminable: Setsid makes the child a session and
// process-group leader (pgid == pid), so the existing Track/Terminate group kill
// (kill -pgid) still tears the whole TREE down, not just the leader. Guards against
// the controlling-tty fix regressing cancel/timeout cleanup.
//
// A childless process cannot distinguish a group kill from a leader kill, so the
// shell leader forks a grandchild and prints its PID. After Terminate the grandchild
// must also be gone: it is reparented to init on the leader's death and reaped there,
// so signalling it settles on ESRCH. A leader-only kill leaves the grandchild alive
// (confirmed out of band), so this assertion genuinely exercises the group kill.
func TestConfigureSessionGroupStillTerminable(t *testing.T) {
	// `sleep 60 &` is the grandchild; `echo $!` reports its PID; `wait` keeps the
	// leader alive until the group is killed.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! ; wait")
	ConfigureSession(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Nuke the whole group in case an assertion failed early, so neither the
		// leader nor a surviving grandchild leaks for the full 60s.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var grandchild int
	if _, err := fmt.Fscan(stdout, &grandchild); err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	if grandchild <= 1 {
		t.Fatalf("implausible grandchild pid %d", grandchild)
	}
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("grandchild %d should be alive before the kill: %v", grandchild, err)
	}

	g, err := Track(cmd, false)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := g.Terminate(syscall.SIGKILL); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected the killed leader to exit non-nil")
	}

	// The group kill must reach the grandchild too; it settles on ESRCH once reaped.
	deadline := time.Now().Add(5 * time.Second)
	for {
		gerr := syscall.Kill(grandchild, 0)
		if errors.Is(gerr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still present after the group kill (Kill(pid,0)=%v): a non-leader member survived", grandchild, gerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
