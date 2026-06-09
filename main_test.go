package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// Build the binary once and use it as its own supervisor against a fake agy.
func TestRunJobSubcommandEndToEnd(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "E2E OK", Exit: 0})

	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(filepath.Join(jobDir, "meta.json"), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run-job: %v\n%s", err, out)
	}
	out, _ := os.ReadFile(filepath.Join(jobDir, "out"))
	if strings.TrimSpace(string(out)) != "E2E OK" {
		t.Fatalf("out = %q", out)
	}
}

// Cancel end to end: SIGTERM the supervisor subprocess and confirm it forwards
// the signal to agy and writes the SIGTERM sentinel, which Status maps to
// "cancelled".
func TestRunJobCancelViaSignal(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// A fake agy that sleeps far longer than the test; with no timeout in meta,
	// only an external cancel signal can stop it.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", SleepSecs: 60})
	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(filepath.Join(jobDir, "meta.json"), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Give the supervisor time to start agy and install its SIGTERM handler.
	time.Sleep(time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(jobDir, "exit_code")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, _ := os.ReadFile(filepath.Join(jobDir, "exit_code"))
	if strings.TrimSpace(string(code)) != strconv.Itoa(jobstore.ExitSIGTERM) {
		t.Fatalf("exit_code = %q, want %d (SIGTERM cancel)", code, jobstore.ExitSIGTERM)
	}
}
