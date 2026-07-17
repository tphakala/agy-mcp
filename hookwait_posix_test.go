//go:build linux || darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// hookPayload builds the stdin JSON hook-wait expects from a PostToolUse hook.
func hookPayload(tool, id string) string {
	return fmt.Sprintf(`{"tool_name":%q,"tool_response":{"job_id":%q,"state":"running"}}`, tool, id)
}

func TestHookWaitWakesOnDoneJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-hw-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var errb bytes.Buffer
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1")), &errb)
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
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run_sync", "job-hw-1")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
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
	code := hookWaitMain([]string{"-timeout", "100ms"}, strings.NewReader(hookPayload("mcp__agy__agy_run", job.ID)), &errb)
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
	code := hookWaitMain(nil, strings.NewReader(hookPayload("mcp__agy__agy_run", "job-none")), &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if errb.String() != "" {
		t.Fatalf("stderr = %q, want empty", errb.String())
	}
}
