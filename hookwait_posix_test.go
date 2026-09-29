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
	if !strings.Contains(errb.String(), "not an error") {
		t.Fatalf("stderr = %q, want it to frame the wake as not an error", errb.String())
	}
}

// TestHookWaitStillRunningNamesSubagentOwner pins the subagent name in the
// still-running wake, the sibling of the finish wake checked by
// TestHookWaitNamesSubagentOwner: a job a subagent started wakes the parent
// either way.
func TestHookWaitStillRunningNamesSubagentOwner(t *testing.T) {
	setFakeHome(t)
	jobID := startRunningJobForWait(t, 2*time.Second, "conv-hookwait-owner-test", 5*time.Second)

	payload := `{"tool_name":"mcp__agy__agy_run","agent_id":"a1","agent_type":"watch-pr","tool_response":{"job_id":"` + jobID + `","state":"running"}}`
	var errb bytes.Buffer
	code := hookWaitMain([]string{"-timeout", "100ms"}, strings.NewReader(payload), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "still running") {
		t.Fatalf("stderr = %q, want the still-running wake", errb.String())
	}
	if !strings.Contains(errb.String(), `started by subagent "watch-pr"`) {
		t.Fatalf("stderr = %q, want it to name the subagent", errb.String())
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
	awaitReady := armWaitReady(t, cmd)
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
	// Block until the child reports its signal handler is installed. Parse and
	// resolveWaitManager run before that point, and a SIGINT landing there would
	// kill the child outright instead of exercising the interrupt path.
	awaitReady()
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
	if !strings.Contains(errb.String(), "not an error") {
		t.Fatalf("stderr = %q, want it to frame the wake as not an error", errb.String())
	}
}

// TestHookWaitWakesOnInterruptDuringCollectedGrace proves a SIGINT that lands
// while hook-wait is looking for the collected marker still produces the
// interrupted wake. The job is already terminal and has no marker, so the wait
// returns at once and hook-wait spends its one-second grace polling; the signal
// is sent partway through it. If the signal handler were removed before the
// grace, the default action would kill the process instead of exiting 2.
func TestHookWaitWakesOnInterruptDuringCollectedGrace(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	cmd := exec.Command(bin, "hook-wait", "-timeout", "1h")
	cmd.Stdin = strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1", "running"))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	awaitReady := armWaitReady(t, cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	awaitReady()
	// The wait on a terminal job returns within one poll; land the signal well
	// inside the one-second grace that follows.
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
