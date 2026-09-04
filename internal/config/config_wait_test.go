package config

import (
	"testing"
)

// TestResolveWaitNeedsNoAgy: ResolveWait must succeed with no agy anywhere on
// PATH, since the wait-only subcommands are pure observers of the job store.
// Untagged so it runs on Windows too, where the wait subcommands ship the same
// guarantee. Resolve tolerates a missing agy as well now (it defers the lookup
// to exec time), so the seam between the two resolvers is pinned by
// TestResolveWaitSkipsAgyLookupWhenPresent instead, with agy actually present.
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

// TestEnvVarNameConstants pins the exported environment-variable names to their
// wire values. They are the user-facing configuration contract, now shared with
// the doctor command's source labels, so a rename is a deliberate change a user
// would feel, not an incidental refactor: this test forces renaming one to be a
// conscious edit here rather than a silent break. Pinning each to a distinct
// reviewed literal also rules out a copy-paste that aimed two settings at the same
// variable, since two equal constants cannot both match their distinct wants.
func TestEnvVarNameConstants(t *testing.T) {
	cases := []struct{ got, want string }{
		{EnvAgyPath, "AGY_MCP_AGY_PATH"},
		{EnvDefaultModel, "AGY_MCP_DEFAULT_MODEL"},
		{EnvStateDir, "AGY_MCP_STATE_DIR"},
		{EnvHTTPToken, "AGY_MCP_HTTP_TOKEN"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("env var constant = %q, want %q", tc.got, tc.want)
		}
	}
}
