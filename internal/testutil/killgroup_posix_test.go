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
		KillProcessGroup(t, pgid, 10*time.Second)
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

// errRecorder captures Errorf calls so a test can assert KillProcessGroup's
// failure branches without failing itself.
type errRecorder struct {
	testing.TB
	errs []string
}

func (r *errRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// A non-positive pgid must be refused before kill(2): kill(-0, ...) would
// signal the caller's own process group. Do not check this test by deleting
// the guard: the resulting kill(0, SIGKILL) kills the test run itself.
func TestKillProcessGroupRefusesNonPositivePgid(t *testing.T) {
	rec := &errRecorder{TB: t}
	KillProcessGroup(rec, 0, time.Second)
	if len(rec.errs) != 1 {
		t.Fatalf("errors = %q, want exactly one refusal", rec.errs)
	}
}

// A group still signalable when the timeout expires fails the test. A negative
// timeout has expired before the first kill, which succeeds because the target
// was running when it was sent.
func TestKillProcessGroupReportsTimeout(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	rec := &errRecorder{TB: t}
	KillProcessGroup(rec, pgid, -time.Nanosecond)
	if len(rec.errs) != 1 {
		t.Fatalf("errors = %q, want exactly one timeout failure", rec.errs)
	}
}
