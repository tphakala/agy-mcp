//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgyOnPath puts an executable named "agy" on PATH and clears the path
// override, so Resolve's PATH lookup succeeds without depending on a real agy.
func fakeAgyOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("AGY_MCP_AGY_PATH", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// On Windows exec.LookPath resolves via PATHEXT, so a bare "agy" is not found;
	// the extra agy.exe makes the helper work there too and is ignored on Unix.
	if err := os.WriteFile(filepath.Join(dir, "agy.exe"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestResolveStateDirOverride: AGY_MCP_STATE_DIR is used verbatim and takes
// precedence over the XDG fallback (no "/agy-mcp" suffix is appended).
func TestResolveStateDirOverride(t *testing.T) {
	t.Setenv("AGY_MCP_STATE_DIR", "/custom/state")
	t.Setenv("XDG_STATE_HOME", "/should/be/ignored")
	fakeAgyOnPath(t)

	c, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/custom/state" {
		t.Errorf("StateDir = %q, want the AGY_MCP_STATE_DIR override verbatim", c.StateDir)
	}
}

// TestResolveDefaultModelAndJobTTL: AGY_MCP_DEFAULT_MODEL flows into the config,
// and JobTTL defaults to 24h (a regression to 0 would silently disable GC).
func TestResolveDefaultModelAndJobTTL(t *testing.T) {
	t.Setenv("AGY_MCP_DEFAULT_MODEL", "Gemini 3.1 Pro (High)")
	t.Setenv("AGY_MCP_STATE_DIR", t.TempDir())
	fakeAgyOnPath(t)

	c, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultModel != "Gemini 3.1 Pro (High)" {
		t.Errorf("DefaultModel = %q, want the AGY_MCP_DEFAULT_MODEL value", c.DefaultModel)
	}
	if c.JobTTL != 24*time.Hour {
		t.Errorf("JobTTL = %v, want the 24h default", c.JobTTL)
	}
}

// TestResolveWaitSkipsAgyLookupWhenPresent pins the seam between the two
// resolvers using the case that still separates them: with agy sitting on PATH,
// Resolve records it and ResolveWait deliberately does not. The wait-only
// subcommands are pure observers of the job store and must never grow a
// dependency on the binary. (The no-agy direction is TestResolveWaitNeedsNoAgy,
// kept untagged so it also covers Windows.)
func TestResolveWaitSkipsAgyLookupWhenPresent(t *testing.T) {
	fakeAgyOnPath(t)
	t.Setenv("AGY_MCP_STATE_DIR", t.TempDir())

	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AgyPath == "" {
		t.Fatal("Resolve left AgyPath empty with agy on PATH; the control condition is broken")
	}
	w, err := ResolveWait()
	if err != nil {
		t.Fatalf("ResolveWait: %v", err)
	}
	if w.AgyPath != "" {
		t.Errorf("wait config resolved a binary it must not need: agy=%q", w.AgyPath)
	}
}

func TestResolveDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	t.Setenv("AGY_MCP_AGY_PATH", "")
	t.Setenv("AGY_MCP_STATE_DIR", "")
	t.Setenv("AGY_MCP_DEFAULT_MODEL", "")
	// Put a fake agy on PATH.
	dir := t.TempDir()
	agy := filepath.Join(dir, "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AgyPath != agy {
		t.Errorf("AgyPath = %q, want %q", c.AgyPath, agy)
	}
	if c.StateDir != "/tmp/xdgstate/agy-mcp" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	if c.DefaultTimeout != 30*time.Minute {
		t.Errorf("DefaultTimeout = %v", c.DefaultTimeout)
	}
	if c.MaxConcurrency != 4 {
		t.Errorf("MaxConcurrency = %d", c.MaxConcurrency)
	}
}

func TestResolveAgyPathOverride(t *testing.T) {
	agy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_MCP_AGY_PATH", agy)
	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AgyPath != agy {
		t.Errorf("AgyPath = %q, want %q", c.AgyPath, agy)
	}
}

func TestResolveAgyPathOverrideMissing(t *testing.T) {
	// A typo'd override should fail fast at startup (symmetric with the PATH-lookup
	// branch) rather than only surfacing as an exec error on the first job.
	t.Setenv("AGY_MCP_AGY_PATH", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve should fail when AGY_MCP_AGY_PATH points at a missing file")
	}
}

func TestResolveAgyPathOverrideMadeAbsolute(t *testing.T) {
	// agy runs under the supervisor with cmd.Dir set to the job's cwd, so a
	// relative AgyPath would resolve against the wrong directory. A relative
	// override must be made absolute at resolution time.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("AGY_MCP_AGY_PATH", "./agy")
	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !filepath.IsAbs(c.AgyPath) {
		t.Errorf("AgyPath = %q, want an absolute path", c.AgyPath)
	}
}

func TestResolveAgyPathOverrideNotExecutable(t *testing.T) {
	// An override that exists but is not executable would fail at exec time, so it
	// must be rejected at startup too (existence alone is not enough).
	agy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(agy, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_MCP_AGY_PATH", agy)
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve should fail when AGY_MCP_AGY_PATH is not executable")
	}
}

// TestResolveWithoutAgyOnPathDefers pins the asymmetry between the two lookup
// branches. An explicit AGY_MCP_AGY_PATH is a claim about a specific binary, so
// a bad one fails fast (see TestResolveAgyPathOverrideMissing). A bare PATH miss
// is not: initialize, tools/list, and list_sessions never exec agy, so refusing
// to start would make the server unusable for introspection (and unrunnable in
// a container that has no agy). Defer that lookup to the first exec instead.
func TestResolveWithoutAgyOnPathDefers(t *testing.T) {
	t.Setenv("AGY_MCP_AGY_PATH", "")
	t.Setenv("AGY_MCP_STATE_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // an empty dir: no agy anywhere on PATH

	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve must succeed without agy on PATH: %v", err)
	}
	if c.AgyPath != "" {
		t.Errorf("AgyPath = %q, want empty so the lookup is retried at exec time", c.AgyPath)
	}
}

func TestAgyBinaryUsesPathResolvedAtStartup(t *testing.T) {
	// A path already resolved by Resolve wins without consulting PATH again: a
	// mid-session PATH change must not silently swap the binary under running jobs.
	t.Setenv("PATH", t.TempDir())
	got, err := Config{AgyPath: "/opt/agy/agy"}.AgyBinary()
	if err != nil {
		t.Fatalf("AgyBinary: %v", err)
	}
	if got != "/opt/agy/agy" {
		t.Errorf("AgyBinary = %q, want the startup-resolved path", got)
	}
}

func TestAgyBinaryLooksUpDeferredPath(t *testing.T) {
	// The deferred case: agy was absent at startup but is present now, so the
	// first job picks it up without needing a server restart.
	dir := t.TempDir()
	agy := filepath.Join(dir, "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := Config{}.AgyBinary()
	if err != nil {
		t.Fatalf("AgyBinary: %v", err)
	}
	if got != agy {
		t.Errorf("AgyBinary = %q, want %q", got, agy)
	}
}

func TestAgyBinaryRejectsRelativePathEntry(t *testing.T) {
	// agy runs under the supervisor with cmd.Dir set to the job's cwd, so a
	// binary found via a relative PATH entry would mean a different program per
	// job. Go's LookPath refuses that outright (exec.ErrDot); the point here is
	// that AgyBinary surfaces the refusal instead of execing something
	// cwd-dependent.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", ".")

	var c Config
	if _, err := c.AgyBinary(); err == nil {
		t.Fatal("AgyBinary must refuse an agy found via a relative PATH entry")
	}
}

func TestAgyBinaryMissingNamesTheOverride(t *testing.T) {
	// The deferred lookup carries the startup error's guidance: a user who never
	// saw a startup failure meets this message on their first job instead.
	t.Setenv("PATH", t.TempDir())

	_, err := Config{}.AgyBinary()
	if err == nil {
		t.Fatal("AgyBinary must fail when agy is not on PATH")
	}
	if !strings.Contains(err.Error(), "AGY_MCP_AGY_PATH") {
		t.Errorf("error should point at the override env var, got: %v", err)
	}
}

func TestResolveHTTPToken(t *testing.T) {
	agy := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_MCP_AGY_PATH", agy) // skip PATH lookup

	t.Run("default empty", func(t *testing.T) {
		t.Setenv("AGY_MCP_HTTP_TOKEN", "")
		c, err := Resolve()
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if c.HTTPToken != "" {
			t.Errorf("HTTPToken = %q, want empty by default", c.HTTPToken)
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Setenv("AGY_MCP_HTTP_TOKEN", "s3cret")
		c, err := Resolve()
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if c.HTTPToken != "s3cret" {
			t.Errorf("HTTPToken = %q, want %q", c.HTTPToken, "s3cret")
		}
	})
}
