package mcptools

import (
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// These tests stage an already finished job on disk instead of running a shell
// script fake agy, so they carry no build tag and also run where the posix job
// tests cannot. The marker code they pin lives in awaitJob and the agy_status
// handler, which no build tag guards (issue #194); the process-driven variants
// are in collected_posix_test.go.

// stateOf returns the state field of a tool result's structured content.
func stateOf(t *testing.T, sc any) any {
	t.Helper()
	m, ok := sc.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want map[string]any: %v", sc, sc)
	}
	return m["state"]
}

// stageTerminalJob writes a finished job into a fresh state dir and returns a
// manager over it with the job's id.
func stageTerminalJob(t *testing.T) (mgr *manager.Manager, jobID string) {
	t.Helper()
	stateDir := t.TempDir()
	jobID = "job-collected-staged-1"
	store := jobstore.New(stateDir)
	dir, err := store.Create(jobstore.Meta{
		ID:        jobID,
		StartedAt: time.Now(),
		Args:      []string{"--output-format", "stream-json"},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("RESULT"), 0o600); err != nil {
		t.Fatalf("write out: %v", err)
	}
	if err := store.WriteExitCode(jobID, 0); err != nil {
		t.Fatalf("write exit code: %v", err)
	}
	mgr = manager.New(config.Config{StateDir: stateDir, DefaultTimeout: time.Minute, MaxConcurrency: 4})
	return mgr, jobID
}

func TestAgyWaitMarksStagedTerminalJobCollected(t *testing.T) {
	mgr, id := stageTerminalJob(t)
	cs := connect(t, mgr, nil)
	if mgr.Collected(id) {
		t.Fatal("collected before any tool returned the outcome")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": id, "wait": "5s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	if st := stateOf(t, res.StructuredContent); st != manager.StateDone {
		t.Fatalf("state = %v, want done", st)
	}
	if !mgr.Collected(id) {
		t.Fatal("agy_wait returned a terminal outcome but did not mark the job collected")
	}
}

func TestAgyStatusMarksStagedTerminalJobCollected(t *testing.T) {
	mgr, id := stageTerminalJob(t)
	cs := connect(t, mgr, nil)
	if mgr.Collected(id) {
		t.Fatal("collected before any tool returned the outcome")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_status",
		Arguments: map[string]any{"job_id": id},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_status: err=%v res=%+v", err, res)
	}
	if st := stateOf(t, res.StructuredContent); st != manager.StateDone {
		t.Fatalf("state = %v, want done", st)
	}
	if !mgr.Collected(id) {
		t.Fatal("agy_status returned a terminal outcome but did not mark the job collected")
	}
}
