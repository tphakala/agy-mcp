//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// hookPayload builds the stdin JSON hook-wait expects from a PostToolUse
// hook, with the given state carried in the tool response (the state the
// tool call itself observed, distinct from whatever the job's state is on
// disk by the time the hook runs).
func hookPayload(tool, id, state string) string {
	return fmt.Sprintf(`{"tool_name":%q,"tool_response":{"job_id":%q,"state":%q}}`, tool, id, state)
}

func TestHookWaitWakesOnDoneJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
// reporting state "running" (no result delivered inline), but by the time
// this hook checks, the job has already finished on disk. The response state
// says "running", so the result was never delivered inline; the wake is
// owed regardless of what the live status now says.
func TestHookWaitWakesOnRunSyncOverrunRace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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

func TestHookWaitWakesOnTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Exit: 0, Sleep: 2 * time.Second})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, "conv-hookwait-timeout-test"),
	})
	stateDir := t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)

	job, err := mgr.StartJob(manager.StartRequest{Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain the started job once the test is done polling hookWaitMain, so a
	// slow fake agy never outlives the test.
	t.Cleanup(func() {
		testutil.WaitFor(t, 5*time.Second, func() bool {
			st, err := mgr.State(job.ID)
			return err == nil && st != manager.StateRunning
		}, "job did not finish before test cleanup")
	})

	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain([]string{"-timeout", "100ms"}, strings.NewReader(hookPayload("mcp__agy__agy_run", job.ID, "running")), &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "still running") {
		t.Fatalf("stderr = %q, want it to mention still running", errb.String())
	}
}

func TestHookWaitQuietOnUnknownJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())

	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Exit: 0, Sleep: 5 * time.Second})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, "conv-hookwait-interrupt-test"),
	})
	stateDir := t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)

	job, err := mgr.StartJob(manager.StartRequest{Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain the started job once the test is done, so the 5s fake agy never
	// outlives the test.
	t.Cleanup(func() {
		testutil.WaitFor(t, 10*time.Second, func() bool {
			st, err := mgr.State(job.ID)
			return err == nil && st != manager.StateRunning
		}, "job did not finish before test cleanup")
	})

	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	cmd := exec.Command(bin, "hook-wait", "-timeout", "1h")
	cmd.Stdin = strings.NewReader(hookPayload("mcp__agy__agy_run", job.ID, "running"))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Signal the child if the test aborts before it is reaped below, so neither it
	// nor its 5s fake agy leaks.
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
