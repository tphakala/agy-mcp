//go:build !windows

package testutil

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// runScriptResult captures the outcome of running a generated test script.
type runScriptResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runScript runs a generated test script (a WriteFakeAgy / WriteFakeSupervisor
// output) under a hard timeout, capturing stdout and stderr separately. A timeout
// fails the test immediately with the captured stderr, so a hung script surfaces
// diagnostics instead of stalling the whole suite. A non-zero exit is returned in
// ExitCode (not a test failure) so callers can assert on it; any other spawn error
// fails the test with stderr included.
func runScript(t *testing.T, timeout time.Duration, path string, args ...string) runScriptResult {
	t.Helper()
	// Derive from t.Context() so the subprocess is bounded by both the timeout and
	// the test's lifetime: if the test ends first, the run is cancelled too.
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("script %s timed out after %s; stderr:\n%s", path, timeout, stderr.String())
	}
	res := runScriptResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		res.ExitCode = ee.ExitCode()
		return res
	}
	t.Fatalf("run script %s: %v; stderr:\n%s", path, err, stderr.String())
	return runScriptResult{}
}
