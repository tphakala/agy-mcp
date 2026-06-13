package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// These tests drive Run, which only supervises on Linux (process groups, signal
// forwarding). They are _linux-gated; the pure tests (resolveExitCode,
// effectiveTimeout) stay in supervisor_test.go so they run everywhere.

// TestSupervisorEscalatesToSIGKILL: when agy ignores SIGTERM, the supervisor
// must escalate to SIGKILL after the grace window. A tiny injected grace
// exercises the escalation in milliseconds instead of the 10s production wait.
func TestSupervisorEscalatesToSIGKILL(t *testing.T) {
	dir := t.TempDir()
	// A fake agy that traps and ignores SIGTERM, so the hard timeout's SIGTERM
	// cannot stop it; only the SIGKILL escalation can.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{IgnoreSIGTERM: true})
	writeMeta(t, dir, jobstore.Meta{
		ID: "j", AgyPath: agy, Args: []string{"-p", "x"},
		StartedAt: time.Now(), Timeout: 300 * time.Millisecond,
	})

	start := time.Now()
	if err := run(dir, 200*time.Millisecond); err != nil {
		t.Fatalf("run: %v", err)
	}
	// With the injected 200ms grace this ends in well under a second; the 10s
	// default would blow past 5s, so this also proves the injected grace was used.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %v; the SIGKILL escalation did not fire promptly", elapsed)
	}
	// A SIGTERM-ignoring agy can only have been stopped by the escalation, and the
	// timeout override maps that kill to the timeout sentinel.
	code, _ := os.ReadFile(jobstore.ExitCodePath(dir))
	if got := strings.TrimSpace(string(code)); got != strconv.Itoa(jobstore.ExitTimeout) {
		t.Fatalf("exit_code = %q, want %d (timeout via SIGKILL escalation)", got, jobstore.ExitTimeout)
	}
}

// TestSupervisorSpawnFailureWrites127: when agy cannot be exec'd, the supervisor
// writes the spawn-failure sentinel and returns the exec error, so Status can
// report "agy did not start" rather than a bare interruption.
func TestSupervisorSpawnFailureWrites127(t *testing.T) {
	dir := t.TempDir()
	writeMeta(t, dir, jobstore.Meta{
		ID: "j", AgyPath: filepath.Join(dir, "nonexistent-agy"), Args: []string{"-p", "x"},
		StartedAt: time.Now(), Timeout: time.Minute,
	})

	if err := Run(dir); err == nil {
		t.Fatal("Run should return the spawn error when agy cannot be exec'd")
	}
	code, _ := os.ReadFile(jobstore.ExitCodePath(dir))
	if got := strings.TrimSpace(string(code)); got != strconv.Itoa(jobstore.ExitSpawnFail) {
		t.Fatalf("exit_code = %q, want %d (spawn failure)", got, jobstore.ExitSpawnFail)
	}
}
