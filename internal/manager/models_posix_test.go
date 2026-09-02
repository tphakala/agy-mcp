//go:build linux || darwin

package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
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
