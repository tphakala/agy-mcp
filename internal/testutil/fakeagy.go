// Package testutil provides test doubles for agy-mcp.
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// FakeAgy configures a stand-in agy binary for tests.
type FakeAgy struct {
	Stdout    string // printed to stdout, then process exits
	Stderr    string // printed to stderr
	Exit      int    // exit code
	SleepSecs int    // seconds to sleep before printing (simulate a long run)
	// IgnoreSIGTERM makes the script trap and ignore SIGTERM and loop forever, so
	// only SIGKILL can stop it. Used to exercise the supervisor's SIGKILL
	// escalation after killGrace. Mutually exclusive with Stdout/Stderr/Exit
	// (the process never reaches them).
	IgnoreSIGTERM bool
}

// WriteFakeAgy writes an executable shell script that mimics agy's print mode
// and returns its path. The script is created under t.TempDir(). The configured
// stdout and stderr are written to sibling payload files and reproduced
// faithfully via cat, so arbitrary byte content (newlines, shell metacharacters)
// survives intact.
func WriteFakeAgy(t *testing.T, cfg FakeAgy) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-agy")

	if cfg.IgnoreSIGTERM {
		// Trap and ignore SIGTERM, then loop forever. The inner sleep is killed by
		// the supervisor's group SIGTERM, but bash ignores it and restarts the
		// loop, so the process survives until the supervisor escalates to SIGKILL.
		script := "#!/usr/bin/env bash\ntrap '' TERM\nwhile :; do sleep 1; done\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake agy: %v", err)
		}
		return path
	}

	outPath := filepath.Join(dir, "fake-agy.out")
	errPath := filepath.Join(dir, "fake-agy.err")
	if err := os.WriteFile(outPath, []byte(cfg.Stdout), 0o644); err != nil {
		t.Fatalf("write fake agy stdout: %v", err)
	}
	if err := os.WriteFile(errPath, []byte(cfg.Stderr), 0o644); err != nil {
		t.Fatalf("write fake agy stderr: %v", err)
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
sleep %d
cat %q
cat %q 1>&2
exit %d
`, cfg.SleepSecs, outPath, errPath, cfg.Exit)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	return path
}
