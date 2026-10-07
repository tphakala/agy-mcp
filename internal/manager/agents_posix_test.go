//go:build linux || darwin

package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
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
	got, err := m.ListAgents(t.Context(), "")
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
		_, err := m.ListAgents(ctx, "")
		return err
	})
}

// newProjectAgentsManager returns a manager over a fake agy whose agents listing
// adds "proj" only when projectDir is passed as --add-dir.
func newProjectAgentsManager(t *testing.T, projectDir string) *Manager {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{
		Agents:        []string{"global"},
		ProjectDir:    projectDir,
		ProjectAgents: []string{"proj"},
	})
	return New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})
}

// TestListAgentsWithCwdListsProjectAgents: a cwd is normalized (symlinks
// resolved, as agy_run does) and passed as --add-dir, so the project's agents
// come back (issue #203).
func TestListAgentsWithCwdListsProjectAgents(t *testing.T) {
	projDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(projDir, link); err != nil {
		t.Fatal(err)
	}
	m := newProjectAgentsManager(t, projDir)

	got, err := m.ListAgents(t.Context(), link)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if want := []string{"global", "proj"}; !slices.Equal(got, want) {
		t.Fatalf("ListAgents(link) = %v, want %v", got, want)
	}
}

// TestListAgentsWithoutCwdAddsNoWorkspace guards the unchanged behaviour: an
// empty cwd passes no --add-dir, unlike agy_run, which defaults an empty cwd to
// the server's directory. The fake would serve the project agents if the server's
// own directory were passed.
func TestListAgentsWithoutCwdAddsNoWorkspace(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	m := newProjectAgentsManager(t, wd)

	got, err := m.ListAgents(t.Context(), "")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if want := []string{"global"}; !slices.Equal(got, want) {
		t.Fatalf("ListAgents(\"\") = %v, want %v", got, want)
	}
}
