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
	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// writeTerminalJob creates <stateDir>/jobs/<id> holding a done job: meta.json
// (CaptureDisabled: true so the wait manager never reads the host's real agy
// conversation cache), an out file "RESULT", and an exit_code sentinel "0".
func writeTerminalJob(t *testing.T, stateDir, id string) {
	t.Helper()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := jobstore.Meta{ID: id, StartedAt: time.Now(), CaptureDisabled: true}
	b, err := jsonMarshalForTest(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.MetaPath(dir), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("RESULT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.ExitCodePath(dir), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWaitJobDoneJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	writeTerminalJob(t, stateDir, "job-done-1")
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var out, errb bytes.Buffer
	code := waitJobMain([]string{"job-done-1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if out.String() != "done\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "done\n")
	}
}

func TestWaitJobUnknownJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var out, errb bytes.Buffer
	code := waitJobMain([]string{"nope"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Fatalf("stderr = %q, want it to mention the failing job", errb.String())
	}
}

func TestWaitJobUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := waitJobMain(nil, &out, &errb); code != 2 {
		t.Fatalf("waitJobMain(nil) = %d, want 2 (stderr: %s)", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := waitJobMain([]string{"a", "b"}, &out, &errb); code != 2 {
		t.Fatalf("waitJobMain(a, b) = %d, want 2 (stderr: %s)", code, errb.String())
	}
}

func TestWaitJobTimeout(t *testing.T) {
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
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, "conv-timeout-test"),
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
	// Drain the started job once the test is done polling waitJobMain, so a
	// slow fake agy never outlives the test.
	t.Cleanup(func() {
		testutil.WaitFor(t, 5*time.Second, func() bool {
			st, err := mgr.State(job.ID)
			return err == nil && st != manager.StateRunning
		}, "job did not finish before test cleanup")
	})

	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	var out, errb bytes.Buffer
	code := waitJobMain([]string{"-timeout", "100ms", job.ID}, &out, &errb)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (stdout: %s, stderr: %s)", code, out.String(), errb.String())
	}
}
