//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHookWaitWakesOnTimeout(t *testing.T) {
	setFakeHome(t)
	jobID := startRunningJobForWait(t, 2*time.Second, "conv-hookwait-timeout-test", 5*time.Second)

	var errb bytes.Buffer
	code := hookWaitMain([]string{"-timeout", "100ms"}, strings.NewReader(hookPayload("mcp__agy__agy_run", jobID, "running")), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "still running") {
		t.Fatalf("stderr = %q, want it to mention still running", errb.String())
	}
}

// TestHookWaitWakesOnInterrupt proves a SIGINT delivered to a waiting hook-wait
// wakes with the distinct interrupt message and exit 2, rather than silently
// exiting 0 and dropping the owed wake. hookWaitMain runs in-process for the
// other tests, but a SIGINT sent to the test binary itself would kill the whole
// run, so this execs the real binary as a child and signals that, mirroring
// TestWaitJobInterrupted.
func TestHookWaitWakesOnInterrupt(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	setFakeHome(t)
	jobID := startRunningJobForWait(t, 5*time.Second, "conv-hookwait-interrupt-test", 10*time.Second)

	cmd := exec.Command(bin, "hook-wait", "-timeout", "1h")
	cmd.Stdin = strings.NewReader(hookPayload("mcp__agy__agy_run", jobID, "running"))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill and reap the child if the test aborts before the happy-path Wait below,
	// so neither it nor its 5s fake agy leaks.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	// Give the child a moment to reach signal.NotifyContext (resolveWaitManager and
	// Parse run first; the job sleeps 5s, so there is ample margin either way)
	// before signalling it.
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	reaped = true
	exitErr, ok := errors.AsType[*exec.ExitError](waitErr)
	if !ok {
		t.Fatalf("hook-wait did not exit with an error after SIGINT: %v (stdout=%q stderr=%q)", waitErr, out.String(), errb.String())
	}
	if code := exitErr.ExitCode(); code != 2 {
		t.Fatalf("exit code = %d, want 2 (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "wait interrupted") {
		t.Fatalf("stderr = %q, want it to mention wait interrupted", errb.String())
	}
}
