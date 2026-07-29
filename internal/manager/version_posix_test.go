//go:build linux || darwin

package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
)

// WaitDelay fires when agy answered and exited but a descendant kept the output
// pipe open. exec reports that as ErrWaitDelay, which is not an *exec.ExitError,
// so treating it as a hard failure rejected a working agy for exactly the reason
// WaitDelay was set. The version it printed is already buffered.
func TestReadAgyVersionToleratesWaitDelay(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	script := filepath.Join(t.TempDir(), "fake-agy")
	// Print the version, leave a descendant holding stdout, exit cleanly.
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 1.1.8\nsleep 30 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := readAgyVersion(t.Context(), script)
	if err != nil {
		t.Fatalf("readAgyVersion: %v; a descendant holding the pipe must not fail the probe", err)
	}
	v, perr := agyver.Parse(raw)
	if perr != nil {
		t.Fatalf("parse %q: %v", raw, perr)
	}
	if !v.AtLeast(agyver.Required) {
		t.Fatalf("version = %v, want the printed 1.1.8", v)
	}
}
