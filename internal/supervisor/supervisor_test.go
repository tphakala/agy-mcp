package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if strings.TrimSpace(string(code)) != "124" {
		t.Fatalf("exit_code = %q, want 124 (timeout)", code)
	}
}
