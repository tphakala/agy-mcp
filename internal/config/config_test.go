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
	t.Setenv("AGY_MCP_AGY_PATH", "/opt/custom/agy")
	c, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.AgyPath != "/opt/custom/agy" {
		t.Errorf("AgyPath = %q", c.AgyPath)
	}
}
