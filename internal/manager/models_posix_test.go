//go:build linux || darwin

package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// writeProbeScript writes a fake agy that answers `--version` cleanly and serves
// the given body for the `--output-format json <cmd>` invocation, exiting 1 for
// anything else. It returns the script path. The models/agents listing paths are
// the two ListModels/ListAgents exercise, and both go through the version gate
// first, so the version answer is mandatory.
func writeProbeScript(t *testing.T, cmd, listingBody string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-agy")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo " + agyver.Required.String() + "; exit 0; fi\n" +
		"if [ \"$1\" = \"--output-format\" ] && [ \"$2\" = \"json\" ] && [ \"$3\" = \"" + cmd + "\" ]; then\n" +
		listingBody +
		"fi\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// reapPidFile kills the descendant whose pid the fake script recorded, so a
// deliberately-orphaned sleep does not linger past the test.
func reapPidFile(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
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
}

// TestListModelsToleratesWaitDelay is the ListModels sibling of
// TestReadAgyVersionToleratesWaitDelay (issue #161): agy printed the envelope and
// exited, but a descendant kept stdout open, so exec reports ErrWaitDelay. That is
// not an *exec.ExitError, so before the fix any Output() error was wrapped and
// returned; the catalog was already buffered and must still decode.
func TestListModelsToleratesWaitDelay(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleeper.pid")
	const envelope = `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"m1","label":"M1"}]}}}`
	// Print the envelope, leave a descendant holding stdout, exit cleanly.
	body := "  printf '%s' '" + envelope + "'\n  sleep 30 &\n  echo $! > \"" + pidFile + "\"\n  exit 0\n"
	agy := writeProbeScript(t, "models", body)
	reapPidFile(t, pidFile)

	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
	got, err := m.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v; a descendant holding the pipe must not fail the listing", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("ListModels() = %#v, want the buffered [m1] catalog", got)
	}
}

// assertListingNamesCancellation drives one JSON listing to cancellation and
// asserts the error names it rather than blaming agy. A ctx-killed listing
// surfaces as an *exec.ExitError just like a non-zero exit, so without the
// ctx.Err() check the message would read "agy <sub>: signal: killed" (issue
// #160). It is shared by the models and agents cases, which differ only in the
// subcommand and the list call.
func assertListingNamesCancellation(t *testing.T, sub string, list func(context.Context, *Manager) error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleeper.pid")
	// The listing branch blocks well past the cancel, so the exec is still running
	// when ctx is cancelled. Record the pid so the sleep is reaped.
	body := "  echo $$ > \"" + pidFile + "\"\n  sleep 30\n  exit 0\n"
	agy := writeProbeScript(t, sub, body)
	reapPidFile(t, pidFile)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
	err := list(ctx, m)
	if err == nil {
		t.Fatalf("agy %s must fail when the caller cancels", sub)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want it to name the cancellation rather than blame agy", err)
	}
}

// TestListModelsNamesCancellation: see assertListingNamesCancellation.
func TestListModelsNamesCancellation(t *testing.T) {
	assertListingNamesCancellation(t, "models", func(ctx context.Context, m *Manager) error {
		_, err := m.ListModels(ctx)
		return err
	})
}

// TestProbeErrorsRedactAgyErrorLines: a probe that fails with an AGY_ERROR line on
// stderr must not copy the cloud project and region its short_error carries into
// the error that list_models, list_agents and agy_usage return (issue #209). The
// plain line before it stays, so the cause is still visible.
func TestProbeErrorsRedactAgyErrorLines(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: measuredAgyErrorStderr, Exit: 3})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"models", func() error { _, err := m.ListModels(t.Context()); return err }},
		{"agents", func() error { _, err := m.ListAgents(t.Context(), ""); return err }},
		{"usage", func() error { _, err := m.readUsage(t.Context()); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("probe succeeded, want an error")
			}
			msg := err.Error()
			for _, leak := range []string{"example-project", "example-region"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q: %s", leak, msg)
				}
			}
			for _, want := range []string{"Selected model is not supported", "AGY_ERROR: NOT_FOUND (code 404)"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q: %s", want, msg)
				}
			}
		})
	}
}

// TestProbeErrorsOmitSeparatorWhenRedactedStderrIsEmpty: a stderr that holds only
// an AGY_ERROR line the reduction drops (here, one whose object does not decode)
// leaves nothing to show, so the error is the bare "agy <label>: exit status N"
// with no dangling separator (issue #209). TestListModelsOmitsWhitespaceOnlyStderr
// cannot tell a guard on the redacted text from one on the raw stderr, since both
// are empty after trimming there.
func TestProbeErrorsOmitSeparatorWhenRedactedStderrIsEmpty(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: wireAgyErrorPrefix + "{not json\n", Exit: 3})

	for _, tc := range []struct {
		name string
		want string
		call func(m *Manager) error
	}{
		{"models", "agy models: exit status 3", func(m *Manager) error { _, err := m.ListModels(t.Context()); return err }},
		{"agents", "agy agents: exit status 3", func(m *Manager) error { _, err := m.ListAgents(t.Context(), ""); return err }},
		{"usage", "agy /usage: exit status 3", func(m *Manager) error { _, err := m.readUsage(t.Context()); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
			err := tc.call(m)
			if err == nil {
				t.Fatal("probe succeeded, want an error")
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestProbeErrorsRedactAcrossCaptureBoundaries: a probe whose stderr is large
// enough that os/exec's own head-and-tail copy would cut an AGY_ERROR line (issue
// #209) must still reach the caller reduced. The first case puts a line across the
// start of the last 32 KiB, where os/exec would keep a prefix-less fragment; the
// second puts it across probeStderrLimit, where the kept part has its prefix but
// not its end and the reduction drops it.
func TestProbeErrorsRedactAcrossCaptureBoundaries(t *testing.T) {
	line := wireAgyErrorPrefix + measuredAgyErrorJSON + "\n"
	const execTail = 32 << 10
	for _, tc := range []struct {
		name   string
		stderr string
		want   string // must remain in the error; empty for none
	}{
		{
			name: "line across the start of os/exec's last 32 KiB",
			stderr: strings.Repeat("x", 70000) + "\n" + line +
				strings.Repeat("y", execTail-len(line)+100) + "\n",
			want: "AGY_ERROR: NOT_FOUND (code 404)",
		},
		{
			name:   "line across probeStderrLimit",
			stderr: strings.Repeat("x", probeStderrLimit-101) + "\n" + line,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: tc.stderr, Exit: 3})
			m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
			_, err := m.ListModels(t.Context())
			if err == nil {
				t.Fatal("probe succeeded, want an error")
			}
			msg := err.Error()
			for _, leak := range []string{"example-project", "example-region"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q", leak)
				}
			}
			if tc.want != "" && !strings.Contains(msg, tc.want) {
				t.Errorf("error lacks %q", tc.want)
			}
			if len(msg) > probeErrorLimit+200 {
				t.Errorf("error is %d bytes, want it bounded near %d", len(msg), probeErrorLimit)
			}
		})
	}
}

// TestProbeErrorDoesNotUnwrapToRawStderr: the ExitError wrapped in a probe error
// carries the reduced stderr, so errors.As cannot recover the project and region
// from it (issue #209).
func TestProbeErrorDoesNotUnwrapToRawStderr(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: measuredAgyErrorStderr, Exit: 3})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.ListModels(t.Context())
	ee, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("err = %v, want it to wrap an *exec.ExitError", err)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	for _, leak := range []string{"example-project", "example-region"} {
		if strings.Contains(string(ee.Stderr), leak) {
			t.Errorf("unwrapped ExitError.Stderr leaks %q: %s", leak, ee.Stderr)
		}
	}
	// Positive control: the copy carries the reduced text, so wrapping the
	// original (whose Stderr is empty once the probe reads stderr itself) fails.
	if want := "AGY_ERROR: NOT_FOUND (code 404)"; !strings.Contains(string(ee.Stderr), want) {
		t.Errorf("unwrapped ExitError.Stderr lacks %q: %q", want, ee.Stderr)
	}
}
