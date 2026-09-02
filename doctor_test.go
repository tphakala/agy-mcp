package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDoctorMainRejectsExtraArgs: doctor takes no positional arguments, so a
// stray one is a usage error (exit 2), not a silently ignored token.
func TestDoctorMainRejectsExtraArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := doctorMain([]string{"unexpected"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2 for an unexpected argument", code)
	}
	if !strings.Contains(errb.String(), "unexpected") {
		t.Errorf("stderr should name the rejected argument, got: %q", errb.String())
	}
}

// TestDoctorMainFailsWithoutAgy: with no agy resolvable, the report has a failing
// check, so the command exits non-zero and prints a FAIL line a script can see.
func TestDoctorMainFailsWithoutAgy(t *testing.T) {
	// Empty PATH so LookPath("agy") cannot succeed; no explicit override; a private
	// state dir so the run never touches real state.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AGY_MCP_AGY_PATH", "")
	t.Setenv("AGY_MCP_STATE_DIR", t.TempDir())

	var out, errb bytes.Buffer
	code := doctorMain(nil, &out, &errb)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when agy is missing; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "[FAIL]") {
		t.Errorf("stdout should carry a FAIL line, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "all checks passed") {
		t.Errorf("stdout must not claim success on a failing report:\n%s", out.String())
	}
}
