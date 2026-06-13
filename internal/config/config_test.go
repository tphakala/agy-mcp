package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	t.Setenv("AGY_MCP_AGY_PATH", "")
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
