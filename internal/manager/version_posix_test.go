//go:build linux || darwin

package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
)

// WaitDelay fires when agy answered and exited but a descendant kept the output
// pipe open. exec reports that as ErrWaitDelay, which is not an *exec.ExitError,
// so treating it as a hard failure rejected a working agy for exactly the reason
// WaitDelay was set. The version it printed is already buffered.
func TestReadAgyVersionToleratesWaitDelay(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-agy")
	// The descendant deliberately outlives the probe, so record its pid and reap
	// it afterwards rather than leaving a 30-second sleep behind on every run.
	pidFile := filepath.Join(dir, "sleeper.pid")
	// Print the version, leave a descendant holding stdout, exit cleanly.
	body := "#!/bin/sh\necho " + agyver.Required.String() + "\nsleep 30 &\necho $! > \"" + pidFile + "\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The script exits before readAgyVersion returns, so by now the pid file
		// is written; a missing or unparsable one means the sleep never started.
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	raw, err := readAgyVersion(t.Context(), script)
	if err != nil {
		t.Fatalf("readAgyVersion: %v; a descendant holding the pipe must not fail the probe", err)
	}
	v, perr := agyver.Parse(raw)
	if perr != nil {
		t.Fatalf("parse %q: %v", raw, perr)
	}
	if !v.AtLeast(agyver.Required) {
		t.Fatalf("version = %v, want the printed %s", v, agyver.Required)
	}
}

// TestReadAgyVersionBoundsTheMergedOutput: an agy that writes megabytes to both
// stdout and stderr must not make readAgyVersion, and the reduction after it, hold
// an unbounded buffer. The output is kept up to probeOutputLimit from the head,
// the version at the start survives, and the call still succeeds (issue #212).
func TestReadAgyVersionBoundsTheMergedOutput(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-agy")
	body := "#!/bin/sh\n" +
		"echo 1.3.1\n" +
		"head -c 2097152 /dev/zero | tr '\\0' x\n" +
		"head -c 2097152 /dev/zero | tr '\\0' y >&2\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := readAgyVersion(t.Context(), script)
	if err != nil {
		t.Fatalf("readAgyVersion: %v", err)
	}
	if len(got) != probeOutputLimit {
		t.Errorf("kept %d bytes, want exactly %d", len(got), probeOutputLimit)
	}
	if !strings.HasPrefix(got, "1.3.1\n") {
		t.Errorf("output does not start with the version: %.20q", got)
	}
}
