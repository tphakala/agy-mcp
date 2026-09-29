package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
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
	// The wake rides Claude Code's "Stop hook blocking error" wrapper, so the body
	// must frame itself as a notification, not a failure. Guard that framing.
	if !strings.Contains(out, "not an error") {
		t.Fatalf("stderr = %q, want it to frame the wake as not an error", out)
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

// shortCollectedGrace keeps tests that expect a wake from paying the full
// production grace before the wake is written.
func shortCollectedGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := collectedGrace
	collectedGrace = d
	t.Cleanup(func() { collectedGrace = old })
}

// TestHookWaitQuietWhenAlreadyCollected covers issue #194: the session already
// collected the finished job (the tool recorded the marker), so the wake carries
// nothing new.
func TestHookWaitQuietWhenAlreadyCollected(t *testing.T) {
	setFakeHome(t)
	shortCollectedGrace(t, 200*time.Millisecond)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)
	if err := jobstore.WriteCollectedDir(filepath.Join(stateDir, "jobs", "job-hw-1")); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1", "running")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
	}
}

// TestHookWaitQuietWhenMarkerLandsInGrace covers the race the marker has by
// construction: awaitJob builds the terminal output and writes the marker
// before it returns, so hook-wait can see the terminal state while the tool has
// the outcome in hand but has not written the marker yet.
func TestHookWaitQuietWhenMarkerLandsInGrace(t *testing.T) {
	setFakeHome(t)
	shortCollectedGrace(t, 5*time.Second)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)
	dir := filepath.Join(stateDir, "jobs", "job-hw-1")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = jobstore.WriteCollectedDir(dir)
	}()

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1", "running")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
}

// TestWaitCollectedStopsWhenContextIsCancelled: the grace must not hold an
// interrupted hook-wait for its full length, and must report the cancellation so
// hookWaitMain takes the interrupted-wake path.
func TestWaitCollectedStopsWhenContextIsCancelled(t *testing.T) {
	setFakeHome(t)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)
	mgr, err := resolveWaitManager()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	collected, err := waitCollected(ctx, mgr, "job-hw-1", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if collected {
		t.Fatal("collected = true for a job with no marker")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waitCollected took %s after cancellation, want it to return promptly", d)
	}
}

// TestHookWaitNamesSubagentOwner: a job started inside a subagent wakes the
// parent, so the wake says whose job it is.
func TestHookWaitNamesSubagentOwner(t *testing.T) {
	setFakeHome(t)
	shortCollectedGrace(t, 50*time.Millisecond)
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	payload := `{"tool_name":"mcp__agy__agy_run","agent_id":"a1","agent_type":"watch-pr","tool_response":{"job_id":"job-hw-1","state":"running"}}`
	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(payload), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), `started by subagent "watch-pr"`) {
		t.Fatalf("stderr = %q, want it to name the subagent", errb.String())
	}
}
