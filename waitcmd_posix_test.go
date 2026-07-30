//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWaitJobTimeout(t *testing.T) {
	setFakeHome(t)
	jobID := startRunningJobForWait(t, 2*time.Second, "conv-timeout-test", 5*time.Second)

	var out, errb bytes.Buffer
	code := waitJobMain([]string{"-timeout", "100ms", jobID}, &out, &errb)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (stdout: %s, stderr: %s)", code, out.String(), errb.String())
	}
}

// TestWaitJobInterrupted proves SIGINT during a wait maps to exit 130
// (128+SIGINT, the POSIX interrupt convention), not the generic error exit 1.
// waitJobMain runs in-process for the other tests, but a SIGINT sent to the
// test binary itself would kill the whole test run, so this execs the real
// binary as a child and signals that instead, mirroring
// TestRunJobCancelViaSignal's approach for run-job.
func TestWaitJobInterrupted(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	setFakeHome(t)
	jobID := startRunningJobForWait(t, 5*time.Second, "conv-interrupt-test", 10*time.Second)

	cmd := exec.Command(bin, "wait-job", "-timeout", "1h", jobID)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	awaitReady := armWaitReady(t, cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill and reap the child if the test aborts before the happy-path Wait below,
	// so neither it nor its fake agy leaks.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	// waitForJob resolves the wait config and constructs the manager before
	// waitForJobWith installs the signal.NotifyContext handler, and a SIGINT
	// arriving in that window kills the child rather than interrupting its wait.
	// Block until the child announces the handler is in place.
	awaitReady()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	reaped = true
	exitErr, ok := errors.AsType[*exec.ExitError](waitErr)
	if !ok {
		t.Fatalf("wait-job did not exit with an error after SIGINT: %v (stdout=%q stderr=%q)", waitErr, out.String(), errb.String())
	}
	if code := exitErr.ExitCode(); code != 130 {
		t.Fatalf("exit code = %d, want 130 (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
}
