package testutil

import (
	"strings"
	"testing"
	"time"
)

func TestFakeAgyEmitsStdoutAndExit(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "hello world", Exit: 0})
	res := runScript(t, 10*time.Second, path, "-p", "ignored")
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello world" {
		t.Fatalf("stdout = %q, want %q", got, "hello world")
	}
}

func TestFakeAgyNonZeroExit(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stderr: "boom", Exit: 3})
	res := runScript(t, 10*time.Second, path)
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3; stderr: %q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stderr); got != "boom" {
		t.Fatalf("stderr = %q, want %q", got, "boom")
	}
}

// TestFakeAgyAppliesFractionalSleep: a sub-second Sleep must be honored as a real
// fractional delay (bash sleep accepts fractions), proving the field is a duration
// rather than whole seconds. Lower bound only, since a sleep is never shorter than
// requested; the generous slack keeps it robust on slow CI.
func TestFakeAgyAppliesFractionalSleep(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "ok", Sleep: 150 * time.Millisecond})
	start := time.Now()
	res := runScript(t, 10*time.Second, path)
	elapsed := time.Since(start)
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("elapsed %s, want >= ~150ms; fractional sleep not applied", elapsed)
	}
}
