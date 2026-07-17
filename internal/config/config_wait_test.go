package config

import (
	"testing"
)

// TestResolveWaitNeedsNoAgy: ResolveWait must succeed with no agy anywhere on
// PATH (the wait-only subcommands are pure observers of the job store), while
// Resolve fails in the same environment, proving the seam matters. Untagged so
// it runs on Windows too, where the wait subcommands ship the same guarantee.
func TestResolveWaitNeedsNoAgy(t *testing.T) {
	// os.UserHomeDir reads HOME on Unix and USERPROFILE on Windows; pin both to
	// temp dirs so the XDG fallback (should Resolve reach it) never touches the
	// real home on either platform.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AGY_MCP_AGY_PATH", "")
	t.Setenv("PATH", t.TempDir()) // empty dir: no agy
	// An absolute state override so the post-absolutization value is stable on
	// every platform (a POSIX-absolute literal is relative on Windows).
	stateDir := t.TempDir()
	t.Setenv("AGY_MCP_STATE_DIR", stateDir)

	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve succeeded without agy on PATH; the control condition is broken")
	}
	c, err := ResolveWait()
	if err != nil {
		t.Fatalf("ResolveWait: %v", err)
	}
	if c.StateDir != stateDir {
		t.Fatalf("StateDir = %q, want %q", c.StateDir, stateDir)
	}
	if c.AgyPath != "" {
		t.Fatalf("wait config resolved a binary it must not need: agy=%q", c.AgyPath)
	}
	// SupervisorExe IS resolved (unlike AgyPath): processAlive's fallback
	// liveness check compares a job's recorded supervisor process name against
	// it, so leaving it empty would make that comparison always fail and
	// misreport a live job as dead.
	if c.SupervisorExe == "" {
		t.Fatal("SupervisorExe = \"\", want the wait config's own executable path")
	}
}
