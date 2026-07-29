package supervisor

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// comspec is the cmd.exe interpreter used to drive the fake agy commands. The
// supervisor execs agy directly and Windows CreateProcess cannot run a .bat, so
// the fake agy is a cmd.exe command line rather than a script file.
func comspec() string {
	if c := os.Getenv("COMSPEC"); c != "" {
		return c
	}
	return "cmd.exe"
}

// writeMetaWin writes meta.json into a job dir for a fake-agy command. agyCmd is
// the cmd.exe command line the supervisor runs (e.g. "echo hi & exit 0").
func writeMetaWin(t *testing.T, dir, agyCmd string, timeout time.Duration) {
	t.Helper()
	m := jobstore.Meta{
		ID:        "j",
		AgyPath:   comspec(),
		Args:      []string{"/c", agyCmd},
		Cwd:       dir,
		StartedAt: time.Now(),
		Timeout:   timeout,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.MetaPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readExit(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(jobstore.ExitCodePath(dir))
	if err != nil {
		t.Fatalf("read exit_code: %v", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse exit_code %q: %v", b, err)
	}
	return code
}

// TestRunCapturesOutputAndExit: a fake agy that prints to stdout and stderr and
// exits non-zero must have its output captured and its exit code recorded.
func TestRunCapturesOutputAndExit(t *testing.T) {
	dir := t.TempDir()
	writeMetaWin(t, dir, "echo review text& echo warn 1>&2& exit 3", time.Minute)
	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, err := os.ReadFile(jobstore.OutPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "review text") {
		t.Errorf("out = %q, want it to contain %q", out, "review text")
	}
	errBytes, err := os.ReadFile(jobstore.ErrPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(errBytes), "warn") {
		t.Errorf("err = %q, want it to contain %q", errBytes, "warn")
	}
	if code := readExit(t, dir); code != 3 {
		t.Errorf("exit_code = %d, want 3", code)
	}
}

// TestRunHardTimeoutTerminatesTree: a fake agy that would run far longer than the
// timeout must be terminated (with its cmd.exe + ping child tree) by the Job
// Object, and the job must record ExitTimeout without waiting out the full run.
func TestRunHardTimeoutTerminatesTree(t *testing.T) {
	dir := t.TempDir()
	// ping -n 60 keeps the tree alive ~60s; the 500ms timeout must cut it short.
	writeMetaWin(t, dir, "ping -n 60 127.0.0.1", 500*time.Millisecond)
	start := time.Now()
	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("Run took %s; the hard timeout did not terminate agy promptly", elapsed)
	}
	if code := readExit(t, dir); code != jobstore.ExitTimeout {
		t.Errorf("exit_code = %d, want %d (timeout)", code, jobstore.ExitTimeout)
	}
}

// TestRunCancelViaSentinel: writing the cancel sentinel while agy is running must
// make the supervisor terminate agy and record ExitSIGTERM (the cancelled state).
func TestRunCancelViaSentinel(t *testing.T) {
	dir := t.TempDir()
	writeMetaWin(t, dir, "ping -n 60 127.0.0.1", time.Minute)

	done := make(chan error, 1)
	go func() { done <- Run(dir) }()

	// Readiness barrier: once out exists, cancel is armed (Run arms waitForCancel
	// before creating the job files), so the sentinel cannot be missed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(jobstore.OutPath(dir)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agy job files never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(jobstore.CancelPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the cancel sentinel was written")
	}
	if code := readExit(t, dir); code != jobstore.ExitSIGTERM {
		t.Errorf("exit_code = %d, want %d (cancelled)", code, jobstore.ExitSIGTERM)
	}
}
