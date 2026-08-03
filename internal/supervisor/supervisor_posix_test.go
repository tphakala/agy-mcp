//go:build linux || darwin

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// These tests drive Run, which supervises on Linux and macOS (process groups,
// signal forwarding); the file is gated linux || darwin. The pure tests
// (resolveExitCode, effectiveTimeout) stay in supervisor_test.go so they run
// everywhere.

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
	if err := run(dir, 200*time.Millisecond, drainGrace); err != nil {
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

// TestSupervisorRecordsSkippedLinesInErrFile drives a malformed agy stream
// through the real supervisor and asserts that the "skipped N unreadable
// line(s)" note persist emits reaches the job's err file, and that the valid
// result after the malformed line still survives. Everywhere else that note is
// asserted only against an in-memory buffer, so this is the one end-to-end check
// that stream corruption is actually surfaced on disk.
func TestSupervisorRecordsSkippedLinesInErrFile(t *testing.T) {
	dir := t.TempDir()
	// A fake agy that emits one undecodable line followed by a valid terminal
	// result. WriteFakeAgy only renders well-formed streams, so this run is staged
	// by hand: the script ignores its own args and prints a fixed stream, which the
	// supervisor execs and decodes from its stdout.
	agy := filepath.Join(t.TempDir(), "malformed-agy")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' 'this is not a json event'\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c-1","status":"SUCCESS","response":"ok"}}'` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(agy, []byte(script), 0o755); err != nil {
		t.Fatalf("write malformed agy: %v", err)
	}
	writeMeta(t, dir, jobstore.Meta{
		ID: "j", AgyPath: agy, Args: []string{"-p", "x"},
		StartedAt: time.Now(), Timeout: time.Minute,
	})

	if err := run(dir, 200*time.Millisecond, drainGrace); err != nil {
		t.Fatalf("run: %v", err)
	}

	errBytes, rerr := os.ReadFile(jobstore.ErrPath(dir))
	if rerr != nil {
		t.Fatalf("read err file: %v", rerr)
	}
	if !strings.Contains(string(errBytes), "skipped 1 unreadable line") {
		t.Fatalf("err file = %q, want it to report the one skipped line", errBytes)
	}

	// The valid result after the malformed line must still have been decoded and
	// written: corruption early in the stream must not discard the answer.
	res, rerr := jobstore.ReadResultDir(dir)
	if rerr != nil || res == nil {
		t.Fatalf("result payload missing after a malformed line: err=%v", rerr)
	}
	var r streamjson.Result
	if err := json.Unmarshal(res, &r); err != nil {
		t.Fatalf("result payload: %v", err)
	}
	if r.Response != "ok" {
		t.Fatalf("result response = %q, want ok", r.Response)
	}
}
