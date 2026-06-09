package mcptools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// connect wires a NewServer(mgr) to a fresh client over in-memory transports
// and returns the client session.
func connect(t *testing.T, mgr *manager.Manager, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	srv := NewServer(mgr)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// newTestManager builds a manager around a fake agy and fake supervisor.
func newTestManager(t *testing.T, fake testutil.FakeAgy) (mgr *manager.Manager, stateDir string) {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	sup := writeFakeSupervisor(t, agy)
	stateDir = t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4}
	return manager.New(c), stateDir
}

// waitForDone polls the manager directly until the job reports done with the
// wanted result, or fails the test at the deadline.
func waitForDone(t *testing.T, mgr *manager.Manager, jobID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := mgr.Status(jobID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.State == manager.StateDone {
			if st.Result != want {
				t.Fatalf("result = %q, want %q", st.Result, want)
			}
			return
		}
		if st.State != manager.StateRunning {
			t.Fatalf("job reached %q, want done", st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not reach done")
}

func TestAgyRunSyncCompletesInline(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "REVIEW OK", Exit: 0})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}
	if sc["result"] != "REVIEW OK" {
		t.Fatalf("result = %v", sc["result"])
	}
	if id, _ := sc["job_id"].(string); id == "" {
		t.Fatal("empty job id")
	}
}

func TestAgyRunSyncOverrunReturnsJobID(t *testing.T) {
	// The sleep must comfortably outlast the 100ms wait cap even on a stalled
	// CI runner, or the job finishes before the cap and the running assertion
	// flakes.
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "LATE OK", Exit: 0, SleepSecs: 5})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "100ms"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["state"] != manager.StateRunning {
		t.Fatalf("state = %v, want running", sc["state"])
	}
	jobID, _ := sc["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}
	if note, _ := sc["note"].(string); note == "" {
		t.Fatal("expected an overrun note")
	}

	// The overrun must not have cancelled the job: it finishes on its own.
	waitForDone(t, mgr, jobID, "LATE OK", 15*time.Second)
}

func TestAgyRunSyncSendsProgress(t *testing.T) {
	// One second spans several 250ms poll ticks, so at least one progress
	// notification fires while the job runs.
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "SLOW OK", Exit: 0, SleepSecs: 1})

	var mu sync.Mutex
	var tokens []any
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			tokens = append(tokens, r.Params.ProgressToken)
			mu.Unlock()
		},
	}
	cs := connect(t, mgr, opts)

	params := &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	}
	params.SetProgressToken("tok-7")
	res, err := cs.CallTool(t.Context(), params)
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	if sc := res.StructuredContent.(map[string]any); sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}

	// Notifications are one-way; give in-flight ones a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(tokens)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) == 0 {
		t.Fatal("no progress notifications received")
	}
	for _, tok := range tokens {
		if tok != "tok-7" {
			t.Fatalf("progress token = %v, want tok-7", tok)
		}
	}
}

func TestAgyRunSyncClientCancelKeepsJobAlive(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "LATE OK", Exit: 0, SleepSecs: 5})
	cs := connect(t, mgr, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "agy_run_sync",
			Arguments: map[string]any{"prompt": "review", "wait": "30s"},
		})
		done <- err
	}()

	// Wait until the job exists on disk, then cancel the call mid-wait.
	jobID := waitForJobDir(t, stateDir, 5*time.Second)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the cancelled call to return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call did not return")
	}

	// Cancelling the call must not cancel the job.
	waitForDone(t, mgr, jobID, "LATE OK", 15*time.Second)
}

// waitForJobDir polls <stateDir>/jobs until exactly one job directory exists
// and returns its id.
func waitForJobDir(t *testing.T, stateDir string, timeout time.Duration) string {
	t.Helper()
	jobs := filepath.Join(stateDir, "jobs")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(jobs)
		if err == nil && len(entries) == 1 && entries[0].IsDir() {
			return entries[0].Name()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job directory did not appear")
	return ""
}
