//go:build linux || darwin

package mcptools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// testConversationID is the id the fake agy reports in its stream for runs
// started by newTestManager.
const testConversationID = "abcdabcd-1234-5678-9abc-def012345678"

// newTestManager builds a manager around a fake agy and a fake supervisor that
// writes the same job-dir files the real one derives from agy's stream. The
// conversation cache is a test-owned empty file: it is no longer where a run's
// id comes from, only where continue_latest and list_sessions look, so pointing
// it away from the real agy cache is all that is needed.
//
// The conversation-id budget is left at zero, which no test needs to opt out of
// unless it is about the budget itself: only agy_run spends it, and no test here
// asserts on a fresh run's conversation_id at a moment when the supervisor may
// not have staged it yet.
func newTestManager(t *testing.T, fake testutil.FakeAgy) (mgr *manager.Manager, stateDir string) {
	t.Helper()
	return newTestManagerWithIDWait(t, fake, 0)
}

// newTestManagerWithIDWait is newTestManager with a real conversation-id budget,
// for the tests that pin which tool pays it.
func newTestManagerWithIDWait(t *testing.T, fake testutil.FakeAgy, idWait time.Duration) (mgr *manager.Manager, stateDir string) {
	t.Helper()
	if fake.ConversationID == "" && !fake.NoConversationID {
		fake.ConversationID = testConversationID
	}
	agy := testutil.WriteFakeAgy(t, fake)
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy, Agy: &fake})
	stateDir = t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationIDWait:    idWait,
		ConversationCacheFile: cachePath}
	return manager.New(c), stateDir
}

// structMap returns a tool result's structured content as a map, failing the
// test (rather than panicking) when the payload is not the expected shape, so a
// response-shape regression reports cleanly instead of crashing the test.
func structMap(t *testing.T, sc any) map[string]any {
	t.Helper()
	m, ok := sc.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want map[string]any: %v", sc, sc)
	}
	return m
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
	sc := structMap(t, res.StructuredContent)
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
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "LATE OK", Exit: 0, Sleep: 2 * time.Second})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "100ms"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
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

// progressCollector returns client options whose progress handler records every
// progress token received, plus a snapshot accessor. Shared by the two
// SendsProgress tests, which differ only in which tool drives the wait.
func progressCollector() (opts *mcp.ClientOptions, snapshot func() []any) {
	var mu sync.Mutex
	var tokens []any
	opts = &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			tokens = append(tokens, r.Params.ProgressToken)
			mu.Unlock()
		},
	}
	snapshot = func() []any {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(tokens)
	}
	return opts, snapshot
}

// assertProgressToken waits briefly for at least one progress notification to
// land (they are one-way, so they can arrive after the call returns) and asserts
// every token received equals want. Shared by the two SendsProgress tests.
func assertProgressToken(t *testing.T, tokens func() []any, want any) {
	t.Helper()
	testutil.WaitFor(t, 2*time.Second, func() bool {
		return len(tokens()) > 0
	}, "no progress notifications received")
	for _, tok := range tokens() {
		if tok != want {
			t.Fatalf("progress token = %v, want %v", tok, want)
		}
	}
}

func TestAgyRunSyncSendsProgress(t *testing.T) {
	// One second spans several 250ms poll ticks, so at least one progress
	// notification fires while the job runs.
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "SLOW OK", Exit: 0, Sleep: 1 * time.Second})

	opts, tokens := progressCollector()
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
	if sc := structMap(t, res.StructuredContent); sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}

	assertProgressToken(t, tokens, "tok-7")
}

func TestAgyRunSyncClientCancelKeepsJobAlive(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "LATE OK", Exit: 0, Sleep: 2 * time.Second})
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

// A fresh run returns its conversation id from agy_run_sync, sourced from agy's
// own stream. A sync caller has no reason to poll again, so an id missing here
// would be lost for good.
func TestAgyRunSyncReturnsConversationID(t *testing.T) {
	const uuid = "12121212-3434-5656-7878-909090909090"
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, ConversationID: uuid})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	if sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}
	if sc["conversation_id"] != uuid {
		t.Fatalf("conversation_id = %v, want %q", sc["conversation_id"], uuid)
	}
	// Token accounting rides along with the terminal result.
	usage, ok := sc["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v, want the accounting object", sc["usage"])
	}
	if usage["total_tokens"] != float64(15) {
		t.Fatalf("usage.total_tokens = %v, want 15", usage["total_tokens"])
	}
}

// killJobGroup arranges for a still-running job's process group to be SIGKILLed
// when the test ends, so a fake agy does not outlive its test and keep writing
// into a TempDir state dir that is being removed.
//
// Cancelling the job does not achieve this, which is why this kills the group
// directly: agy_cancel signals the supervisor pid alone (proc.Signal is explicitly
// "a single pid, not its group"), and the fake supervisor traps nothing, so it
// dies at once and orphans the fake agy it was running in the foreground. The
// manager package's killJob exists for the same reason. Registering a Cleanup
// rather than calling this at the end of a test body also means it runs on the
// t.Fatalf paths, which is where a leaked job is most likely.
func killJobGroup(t *testing.T, stateDir, jobID string) {
	t.Helper()
	t.Cleanup(func() {
		// No readable meta means no pid to signal: the job either never recorded
		// one or is already gone. ESRCH on an exited group is the ordinary case,
		// so the Kill error is discarded.
		meta, err := jobstore.LoadDir(filepath.Join(stateDir, "jobs", jobID))
		if err != nil || meta.PID <= 0 {
			return
		}
		_ = syscall.Kill(-meta.PID, syscall.SIGKILL)
	})
}

// agy_run_sync must not pay the conversation-id budget. It discards the value:
// its own conversation_id comes off the Status it polls anyway, which
// TestAgyRunSyncReturnsConversationID pins. While the wait lived in StartJob,
// agy_run_sync paid it before its own inline-wait deadline was even set, so a
// call that outran the deadline overran its documented wait cap by the budget.
func TestAgyRunSyncDoesNotPayTheConversationIDBudget(t *testing.T) {
	// Names no conversation, so no id can end the wait early, and outlives the
	// 100ms cap by far, so the call is decided by the cap rather than by the run
	// finishing. The run does end before the 30s budget would, through the
	// terminal-job exit; what this measures is that the budget is not on the path
	// at all, not that it runs to its full length.
	mgr, stateDir := newTestManagerWithIDWait(t, testutil.FakeAgy{
		Stdout: "LATE OK", Exit: 0, Sleep: 20 * time.Second, NoConversationID: true,
	}, 30*time.Second)
	cs := connect(t, mgr, nil)

	start := time.Now()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "100ms"},
	})
	elapsed := time.Since(start)
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	jobID, _ := sc["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}
	killJobGroup(t, stateDir, jobID)
	// Checked first, because it is the property under test and its message names
	// the cause. A wait left on this path also drags the run past the cap, which
	// trips the state assertion below with a less useful diagnosis.
	// Generous against a stalled runner, and still far below what this call takes
	// with the wait restored: measured at 20.5s, where the run's own end releases
	// it through the terminal-job exit before the 30s budget could.
	if elapsed > 5*time.Second {
		t.Fatalf("agy_run_sync took %s for a 100ms wait; it is paying the conversation-id budget", elapsed)
	}
	// Pin the overrun shape too, so the elapsed bound cannot be satisfied by a
	// handler that never waited, never started the job, or ignored the cap.
	if sc["state"] != manager.StateRunning {
		t.Fatalf("state = %v, want running: the 100ms cap must expire on a 20s run", sc["state"])
	}
	if note, _ := sc["note"].(string); note == "" {
		t.Fatal("expected an overrun note alongside the running state")
	}
}

// The usage object is copied field by field into a separate wire struct, so a
// transposed pair compiles and passes every test that only checks a total.
// FakeAgy emits distinct values precisely so this can catch that.
func TestAgyRunSyncReportsFullUsage(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	usage, ok := sc["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v, want the accounting object", sc["usage"])
	}
	for field, want := range map[string]float64{
		"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
	} {
		if usage[field] != want {
			t.Errorf("usage[%q] = %v, want %v", field, usage[field], want)
		}
	}
	if sc["num_turns"] != float64(1) {
		t.Errorf("num_turns = %v, want 1", sc["num_turns"])
	}
}
