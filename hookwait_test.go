package main

import (
	"bytes"
	"strings"
	"testing"
)

// The file-based hook-wait tests live in this untagged file (not the posix one)
// so they run on Windows CI too; they only read and write files and never exec a
// shell fake or send a signal. The shell-driven timeout and signal-interrupt
// tests stay in hookwait_posix_test.go.

func TestHookWaitWakesOnDoneJob(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1", "running")), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	out := errb.String()
	if !strings.Contains(out, "job-hw-1") {
		t.Fatalf("stderr = %q, want it to mention the job id", out)
	}
	if !strings.Contains(out, "state=done") {
		t.Fatalf("stderr = %q, want it to mention state=done", out)
	}
	if !strings.Contains(out, "agy_status") {
		t.Fatalf("stderr = %q, want it to mention agy_status", out)
	}
}

func TestHookWaitQuietWhenRunSyncAlreadyTerminal(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run_sync", "job-hw-1", "done")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
	}
}

// TestHookWaitWakesOnRunSyncOverrunRace covers the overrun-then-finished race:
// a run_sync call overran its wait cap and returned with the response still
// reporting state "running" (no result delivered inline), but by the time this
// hook checks, the job has already finished on disk. The response state says
// "running", so the result was never delivered inline; the wake is owed
// regardless of what the live status now says.
func TestHookWaitWakesOnRunSyncOverrunRace(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run_sync", "job-hw-1", "running")), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "job-hw-1") {
		t.Fatalf("stderr = %q, want it to mention the job id", errb.String())
	}
}

// TestHookWaitWakesOnRunSyncMissingState covers a run_sync response that carries
// a job_id but no state field. An absent state is not proof the result was
// delivered inline, so it must fail toward waking (exit 2) rather than suppress
// an owed wake, even though the job is already terminal on disk.
func TestHookWaitWakesOnRunSyncMissingState(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	payload := `{"tool_name":"mcp__agy__agy_run_sync","tool_response":{"job_id":"job-hw-1"}}`
	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(payload), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "job-hw-1") {
		t.Fatalf("stderr = %q, want it to mention the job id", errb.String())
	}
}

func TestHookWaitQuietOnNoJobID(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(`{"tool_name":"mcp__agy__agy_run","tool_response":{"error":"x"}}`), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
	}
}

func TestHookWaitQuietOnUnknownJob(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-none", "running")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
	}
}
