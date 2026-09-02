//go:build linux || darwin

package manager

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
)

// TestListAgentsToleratesWaitDelay mirrors TestListModelsToleratesWaitDelay for
// the agents listing: agy printed the envelope and exited, a descendant kept
// stdout open (ErrWaitDelay), and the already-buffered catalog must still decode.
func TestListAgentsToleratesWaitDelay(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleeper.pid")
	const envelope = `{"status":"SUCCESS","command":{"name":"agents","data":{"agents":["reviewer"]}}}`
	body := "  printf '%s' '" + envelope + "'\n  sleep 30 &\n  echo $! > \"" + pidFile + "\"\n  exit 0\n"
	agy := writeProbeScript(t, "agents", body)
	reapPidFile(t, pidFile)

	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
	got, err := m.ListAgents(t.Context())
	if err != nil {
		t.Fatalf("ListAgents: %v; a descendant holding the pipe must not fail the listing", err)
	}
	if len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("ListAgents() = %#v, want the buffered [reviewer] catalog", got)
	}
}

// TestListAgentsNamesCancellation: see assertListingNamesCancellation.
func TestListAgentsNamesCancellation(t *testing.T) {
	assertListingNamesCancellation(t, "agents", func(ctx context.Context, m *Manager) error {
		_, err := m.ListAgents(ctx)
		return err
	})
}
