package main

import (
	"bytes"
	"strings"
	"testing"
)

// The file-based wait-job tests live in this untagged file (not the posix one)
// so they run on Windows CI too; they only read and write files and never exec a
// shell fake or send a signal.

func TestWaitJobDoneJob(t *testing.T) {
	setFakeHome(t)
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
	setFakeHome(t)
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
	setFakeHome(t)
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
