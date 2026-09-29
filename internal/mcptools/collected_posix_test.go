//go:build linux || darwin

package mcptools

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// startJob runs agy_run and returns the job id.
func startJob(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, res)
	}
	id, _ := structMap(t, res.StructuredContent)["job_id"].(string)
	if id == "" {
		t.Fatal("empty job id")
	}
	return id
}

// The collected marker (issue #194) is what lets hook-wait skip a wake the
// session no longer needs. Each tool that hands over a terminal outcome must
// record it, and none may record it for a job that is still running.

func TestAgyWaitMarksTerminalJobCollected(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 300 * time.Millisecond})
	cs := connect(t, mgr, nil)
	id := startJob(t, cs)
	if mgr.Collected(id) {
		t.Fatal("collected before any tool returned the outcome")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": id, "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	if !mgr.Collected(id) {
		t.Fatal("agy_wait returned a terminal outcome but did not mark the job collected")
	}
}

func TestAgyWaitOverrunDoesNotMarkCollected(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 5 * time.Second})
	cs := connect(t, mgr, nil)
	id := startJob(t, cs)
	waitForRunningJob(t, mgr, stateDir, 5*time.Second)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": id, "wait": "200ms"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	if st := structMap(t, res.StructuredContent)["state"]; st != manager.StateRunning {
		t.Fatalf("state = %v, want running", st)
	}
	if mgr.Collected(id) {
		t.Fatal("a wait that overran left the job running yet marked it collected, which would suppress an owed wake")
	}
}

func TestAgyStatusMarksOnlyTerminalJobCollected(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 800 * time.Millisecond})
	cs := connect(t, mgr, nil)
	id := startJob(t, cs)
	waitForRunningJob(t, mgr, stateDir, 5*time.Second)
	call := func() string {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "agy_status",
			Arguments: map[string]any{"job_id": id},
		})
		if err != nil || res.IsError {
			t.Fatalf("agy_status: err=%v res=%+v", err, res)
		}
		st, _ := structMap(t, res.StructuredContent)["state"].(string)
		return st
	}
	if st := call(); st != manager.StateRunning {
		t.Fatalf("state = %q, want running", st)
	}
	if mgr.Collected(id) {
		t.Fatal("agy_status on a running job marked it collected")
	}
	waitForDone(t, mgr, id, "OK", 10*time.Second)
	if st := call(); st != manager.StateDone {
		t.Fatalf("state = %q, want done", st)
	}
	if !mgr.Collected(id) {
		t.Fatal("agy_status returned a terminal outcome but did not mark the job collected")
	}
}

// A cancel response carries only a state, not the job's outcome, and the job can
// still finish on its own after the cancel is requested. The wake stays owed, so
// agy_cancel must leave the marker absent.
func TestAgyCancelDoesNotMarkCollected(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 30 * time.Second})
	cs := connect(t, mgr, nil)
	id := startJob(t, cs)
	waitForRunningJob(t, mgr, stateDir, 5*time.Second)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_cancel",
		Arguments: map[string]any{"job_id": id},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_cancel: err=%v res=%+v", err, res)
	}
	if mgr.Collected(id) {
		t.Fatal("agy_cancel marked the job collected before its outcome was known, which would suppress an owed wake")
	}
}

// Cancelling a job that already finished returns none of its result, so the
// finish wake is still owed and the job must not be marked collected.
func TestAgyCancelOnFinishedJobDoesNotMarkCollected(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	cs := connect(t, mgr, nil)
	id := startJob(t, cs)
	waitForDone(t, mgr, id, "OK", 10*time.Second)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_cancel",
		Arguments: map[string]any{"job_id": id},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_cancel: err=%v res=%+v", err, res)
	}
	if mgr.Collected(id) {
		t.Fatal("cancel of an already finished job marked it collected, which would suppress an owed wake")
	}
}
