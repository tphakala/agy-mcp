package mcptools

import (
	"context"
	"errors"
	"fmt"
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

// testConversationID is the id the fake supervisor's cache write attributes to
// fresh runs started by newTestManager.
const testConversationID = "abcdabcd-1234-5678-9abc-def012345678"

// newTestManager builds a manager around a fake agy and fake supervisor. The
// fake supervisor writes a conversation cache entry for the test's cwd after
// the exit sentinel, so fresh runs capture an id the way real runs do, against
// a test-owned cache file rather than the real agy cache.
func newTestManager(t *testing.T, fake testutil.FakeAgy) (mgr *manager.Manager, stateDir string) {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, testConversationID),
	})
	stateDir = t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
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

	// No progress token is set on the call, so no notification may arrive.
	var mu sync.Mutex
	var notified int
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notified++
			mu.Unlock()
		},
	}
	cs := connect(t, mgr, opts)

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
	mu.Lock()
	defer mu.Unlock()
	if notified != 0 {
		t.Fatalf("got %d progress notifications without a progress token", notified)
	}
}

func TestAgyRunSyncRejectsInvalidWait(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "x", Exit: 0})
	cs := connect(t, mgr, nil)

	for _, wait := range []string{"nope", "-1s", "0"} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "agy_run_sync",
			Arguments: map[string]any{"prompt": "review", "wait": wait},
		})
		if err == nil && !res.IsError {
			t.Errorf("wait %q: expected an error, got %+v", wait, res.StructuredContent)
		}
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

	// Wait until the job is observably running, then cancel the call mid-wait.
	jobID := waitForRunningJob(t, mgr, stateDir, 5*time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled call returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call did not return")
	}

	// Cancelling the call must not cancel the job.
	waitForDone(t, mgr, jobID, "LATE OK", 15*time.Second)
}

// waitForRunningJob polls <stateDir>/jobs until a job directory exists AND the
// manager reports it running, then returns its id. Waiting for the directory
// alone races StartJob's PID persistence: in that window Status reports a
// transient "interrupted" failure for a perfectly healthy job.
func waitForRunningJob(t *testing.T, mgr *manager.Manager, stateDir string, timeout time.Duration) string {
	t.Helper()
	jobs := filepath.Join(stateDir, "jobs")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if entries, err := os.ReadDir(jobs); err == nil && len(entries) == 1 && entries[0].IsDir() {
			id := entries[0].Name()
			if st, err := mgr.Status(id); err == nil && st.State == manager.StateRunning {
				return id
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not reach running")
	return ""
}

// A fresh run whose conversation cache lands only after the exit sentinel (the
// real-world ordering: agy's cache daemon flushes after the process exits) must
// still return its conversation id from agy_run_sync. Returning done with no id
// loses the id for good, because a sync caller has no reason to poll again.
func TestAgyRunSyncReturnsLateCapturedConversationID(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const uuid = "12121212-3434-5656-7878-909090909090"
	// CacheDelay (700ms) must stay below the manager's default captureBudget (2s)
	// so the completion goroutine is still retrying the capture when the cache
	// lands; otherwise the id would be lost and this test would pass for the wrong
	// reason. The mcptools package cannot set the unexported captureBudget, so this
	// dependency is documented rather than pinned.
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:    agy,
		CachePath:  cachePath,
		CacheJSON:  fmt.Sprintf(`{%q:%q}`, cwd, uuid),
		CacheDelay: 700 * time.Millisecond,
	})
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: t.TempDir(),
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)
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
	if sc["conversation_id"] != uuid {
		t.Fatalf("conversation_id = %v, want %q (the id must not be lost to cache-flush lag)",
			sc["conversation_id"], uuid)
	}
}
