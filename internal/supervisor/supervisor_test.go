package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func writeMeta(t *testing.T, dir string, m jobstore.Meta) {
	t.Helper()
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveExitCode(t *testing.T) {
	cases := []struct {
		name       string
		raw        int
		waitFailed bool
		timedOut   bool
		cancelled  bool
		want       int
	}{
		{"natural success is untouched", 0, false, false, false, 0},
		{"natural failure is untouched", 5, true, false, false, 5},
		{"crash with no supervisor termination stays a failure", 128 + 11, true, false, false, 128 + 11},
		{"timeout that died by SIGTERM reports timeout", jobstore.ExitSIGTERM, true, true, false, jobstore.ExitTimeout},
		{"timeout that escalated to SIGKILL reports timeout", 128 + 9, true, true, false, jobstore.ExitTimeout},
		{"cancel that died by SIGTERM reports cancel", jobstore.ExitSIGTERM, true, false, true, jobstore.ExitSIGTERM},
		{"cancel that escalated to SIGKILL still reports cancel", 128 + 9, true, false, true, jobstore.ExitSIGTERM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveExitCode(tc.raw, tc.waitFailed, tc.timedOut, tc.cancelled); got != tc.want {
				t.Errorf("resolveExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveTimeout(t *testing.T) {
	if got := effectiveTimeout(0); got != fallbackTimeout {
		t.Errorf("effectiveTimeout(0) = %v, want fallback %v", got, fallbackTimeout)
	}
	if got := effectiveTimeout(-5 * time.Second); got != fallbackTimeout {
		t.Errorf("effectiveTimeout(negative) = %v, want fallback %v", got, fallbackTimeout)
	}
	if got := effectiveTimeout(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("effectiveTimeout(5m) = %v, want passthrough", got)
	}
}

func TestSupervisorCapturesOutputAndSentinel(t *testing.T) {
	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "review text", Stderr: "warn", Exit: 0})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, Args: []string{"-p", "x"}, StartedAt: time.Now()})

	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "out"))
	if strings.TrimSpace(string(out)) != "review text" {
		t.Fatalf("out = %q", out)
	}
	code, err := os.ReadFile(filepath.Join(dir, "exit_code"))
	if err != nil || strings.TrimSpace(string(code)) != "0" {
		t.Fatalf("exit_code = %q, err=%v", code, err)
	}
}

func TestSupervisorRecordsNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: "fail", Exit: 7})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, StartedAt: time.Now()})
	if err := Run(dir); err != nil {
		t.Fatalf("Run should not error on non-zero agy exit: %v", err)
	}
	code, _ := os.ReadFile(filepath.Join(dir, "exit_code"))
	if strings.TrimSpace(string(code)) != "7" {
		t.Fatalf("exit_code = %q, want 7", code)
	}
}

func TestSupervisorHardTimeoutKillsAgy(t *testing.T) {
	dir := t.TempDir()
	// A fake agy that would sleep far longer than the hard timeout.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "should not finish", SleepSecs: 30})
	writeMeta(t, dir, jobstore.Meta{
		ID: "j", AgyPath: agy, Args: []string{"-p", "x"},
		StartedAt: time.Now(), Timeout: 500 * time.Millisecond,
	})

	start := time.Now()
	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %v; the hard timeout did not kill agy", elapsed)
	}
	code, _ := os.ReadFile(filepath.Join(dir, "exit_code"))
	if got := strings.TrimSpace(string(code)); got != strconv.Itoa(jobstore.ExitTimeout) {
		t.Fatalf("exit_code = %q, want %d (timeout)", got, jobstore.ExitTimeout)
	}
}
