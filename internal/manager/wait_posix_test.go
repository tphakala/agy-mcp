//go:build linux || darwin

package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// waitManager builds a Manager whose jobs run the given fake agy under a fake
// supervisor. The supervisor writes no conversation cache and the cache file is
// an empty object, so a fresh run's id capture arms and then settles empty
// within a short capture budget. That keeps WaitTerminal's capture-grace path
// (done + CapturePending) from stalling the tests that do not target it.
func waitManager(t *testing.T, fake testutil.FakeAgy) *Manager {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy})
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{
		agyPath:        agy,
		supervisorExe:  sup,
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
	})
	m.cacheFile = cachePath
	m.captureBudget = 50 * time.Millisecond
	m.capturePoll = 10 * time.Millisecond
	return m
}

// drainJob waits for a still-running job to finish, so the test's TempDir
// StateDir is not removed out from under a supervisor still writing into it.
func drainJob(t *testing.T, m *Manager, id string) {
	t.Helper()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		st, err := m.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		return st.State == StateDone
	}, "job never finished")
}

// A job that is already done when WaitTerminal is called must return its
// terminal status without waiting.
func TestWaitTerminalReturnsDoneJob(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// Poll until the job is done and its id capture has settled, so the call
	// below does not enter the capture-grace path and can return at once.
	testutil.WaitFor(t, 3*time.Second, func() bool {
		st, err := m.Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != StateRunning && st.State != StateDone {
			t.Fatalf("job reached %q, want running or done", st.State)
		}
		return st.State == StateDone && !m.CapturePending(job.ID)
	}, "job never settled done")

	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(5*time.Second), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if !terminal {
		t.Fatal("terminal = false, want true")
	}
	if st.Result != "OK" {
		t.Fatalf("result = %q, want OK", st.Result)
	}
}

// WaitTerminal must block until a running job reaches a terminal state, then
// report its captured result.
func TestWaitTerminalBlocksUntilDone(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 1 * time.Second})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(15*time.Second), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateDone {
		t.Fatalf("state = %q terminal = %v, want done/true", st.State, terminal)
	}
	if st.Result != "OK" {
		t.Fatalf("result = %q, want OK", st.Result)
	}
}

// A deadline that passes while the job is still running must end the wait with a
// non-terminal running status and no error.
func TestWaitTerminalDeadlineOverrun(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 2 * time.Second})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(100*time.Millisecond), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if terminal {
		t.Fatal("terminal = true, want false on deadline overrun")
	}
	if st.State != StateRunning {
		t.Fatalf("state = %q, want running", st.State)
	}
	drainJob(t, m, job.ID)
}

// A cancelled context must end the wait with context.Canceled and a
// non-terminal result, without touching the still-running job.
func TestWaitTerminalContextCancel(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 2 * time.Second})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel only once WaitTerminal is observably polling, so the cancellation
	// races the running-state select rather than the first status read.
	time.AfterFunc(300*time.Millisecond, cancel)

	_, terminal, err := m.WaitTerminal(ctx, job.ID, time.Now().Add(15*time.Second), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if terminal {
		t.Fatal("terminal = true, want false on cancel")
	}
	drainJob(t, m, job.ID)
}

// An unknown job id must surface the store load failure, not a terminal result.
func TestWaitTerminalUnknownJob(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	_, terminal, err := m.WaitTerminal(t.Context(), "nonexistent-id", time.Now().Add(5*time.Second), nil)
	if err == nil {
		t.Fatal("err = nil, want a store load failure for an unknown job")
	}
	if terminal {
		t.Fatal("terminal = true, want false for an unknown job")
	}
}

// onTick must fire at least once while the job runs, and every status it
// observes must be running.
func TestWaitTerminalOnTick(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 1 * time.Second})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// onTick is documented to run only from WaitTerminal's own goroutine, so these
	// need no synchronization.
	var ticks int
	var sawNonRunning bool
	onTick := func(st Status) {
		ticks++
		if st.State != StateRunning {
			sawNonRunning = true
		}
	}
	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(15*time.Second), onTick)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateDone {
		t.Fatalf("state = %q terminal = %v, want done/true", st.State, terminal)
	}
	if ticks < 1 {
		t.Fatalf("onTick fired %d times, want >= 1", ticks)
	}
	if sawNonRunning {
		t.Fatal("onTick observed a non-running status; it must only fire while running")
	}
}

// A fresh run whose conversation cache lands only after the exit sentinel (agy's
// cache daemon flushes after the process exits) must still return its id from
// WaitTerminal: the capture grace must hold until the late capture settles
// rather than returning done with an empty id. This mirrors
// TestAgyRunSyncReturnsLateCapturedConversationID at the manager level.
func TestWaitTerminalGraceDeliversLateCapturedID(t *testing.T) {
	// See TestFreshRunCapturesConversationID: the cache key must match the
	// symlink-resolved path StartJob persists as meta.Cwd, or the fake
	// supervisor's cache write is never attributed to this run.
	cwd, err := normalizeCwd(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const uuid = "13131313-2424-3535-4646-575757575757"

	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	// The cache lands ~600ms after the exit sentinel, mimicking agy's cache-daemon
	// lag. The manager's default captureBudget (2s) stays comfortably larger, so
	// the completion goroutine is still retrying the capture when the cache write
	// arrives; WaitTerminal's grace must span that window. Using the default
	// budget (not the short waitManager one) is what exercises the grace path.
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:    agy,
		CachePath:  cachePath,
		CacheJSON:  fmt.Sprintf(`{%q:%q}`, cwd, uuid),
		CacheDelay: 600 * time.Millisecond,
	})
	m := newManager(t, managerOpts{
		agyPath:        agy,
		supervisorExe:  sup,
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
	})
	m.cacheFile = cachePath

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: cwd})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(15*time.Second), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateDone {
		t.Fatalf("state = %q terminal = %v, want done/true", st.State, terminal)
	}
	if st.ConversationID != uuid {
		t.Fatalf("conversation_id = %q, want %q (grace must hold until the late capture lands)", st.ConversationID, uuid)
	}
}

// A waiter in a DIFFERENT process than the server that owns the job (hook-wait,
// wait-job) never sees CapturePending, so without the completion-recency grace it
// would return done with an empty id the instant the exit sentinel appears, while
// the owning server is still inside its capture retry. The cross-process grace
// must hold such a waiter until the late cache flush lands, at which point its own
// Status lazy-captures the id.
func TestWaitTerminalCrossProcessGraceDeliversLateID(t *testing.T) {
	// See TestFreshRunCapturesConversationID: the cache key must match the
	// symlink-resolved path StartJob persists as meta.Cwd.
	cwd, err := normalizeCwd(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const uuid = "24242424-3535-4646-5757-686868686868"

	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	// The cache lands ~700ms after the exit sentinel. B must begin waiting inside
	// that gap, with the job done but no id yet, to prove its grace holds until the
	// late capture arrives rather than returning done with an empty id.
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:    agy,
		CachePath:  cachePath,
		CacheJSON:  fmt.Sprintf(`{%q:%q}`, cwd, uuid),
		CacheDelay: 700 * time.Millisecond,
	})

	// Manager A owns and runs the job (the MCP server process).
	mA := newManager(t, managerOpts{
		agyPath: agy, supervisorExe: sup, stateDir: stateDir,
		defaultTimeout: time.Minute, maxConcurrency: 4,
	})
	mA.cacheFile = cachePath

	job, err := mA.StartJob(StartRequest{Prompt: "hi", Cwd: cwd})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// Wait only for the exit sentinel, NOT for A's capture: B must start waiting
	// while the job is done but the cache has not yet landed.
	testutil.WaitFor(t, 3*time.Second, func() bool {
		_, ok := mA.store.ExitCode(job.ID)
		return ok
	}, "job never wrote its exit sentinel")

	// Manager B is a distinct process's view: same state dir and cache file, fresh
	// in-memory maps, so CapturePending and captureSettled are always false for this
	// job. Its grace rests entirely on the completion-recency window.
	mB := newManager(t, managerOpts{
		agyPath: agy, supervisorExe: sup, stateDir: stateDir,
		defaultTimeout: time.Minute, maxConcurrency: 4,
	})
	mB.cacheFile = cachePath
	if mB.CapturePending(job.ID) {
		t.Fatal("cross-process manager must not see the job as capture-pending")
	}

	st, terminal, err := mB.WaitTerminal(t.Context(), job.ID, time.Now().Add(15*time.Second), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateDone {
		t.Fatalf("state = %q terminal = %v, want done/true", st.State, terminal)
	}
	if st.ConversationID != uuid {
		t.Fatalf("conversation_id = %q, want %q (cross-process grace must hold until the late capture lands)", st.ConversationID, uuid)
	}
}

// A fresh run whose capture was disabled at start (a torn pre-run snapshot) can
// never produce a conversation id, so the recency grace must be skipped: a
// cross-process waiter must return at once rather than stall for the full
// captureGraceWindow waiting for an id that is not coming.
func TestWaitTerminalNoGraceForDisabledCapture(t *testing.T) {
	stateDir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A completed fresh run left on disk with capture disabled and a clean exit.
	m := newManager(t, managerOpts{
		agyPath: "/usr/bin/agy", supervisorExe: "/bin/true", stateDir: stateDir,
		defaultTimeout: time.Minute, maxConcurrency: 4,
	})
	m.cacheFile = cachePath
	meta := jobstore.Meta{
		ID:              "job-disabled",
		Cwd:             t.TempDir(),
		CaptureDisabled: true,
		StartedAt:       time.Now().Add(-time.Second),
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("OK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
		t.Fatal(err)
	}

	// A distinct process's view: fresh maps, so CapturePending and captureConcluded
	// are both false. Only the CaptureDisabled short-circuit keeps this from polling
	// the full captureGraceWindow.
	mB := newManager(t, managerOpts{
		agyPath: "/usr/bin/agy", supervisorExe: "/bin/true", stateDir: stateDir,
		defaultTimeout: time.Minute, maxConcurrency: 4,
	})
	mB.cacheFile = cachePath

	start := time.Now()
	st, terminal, err := mB.WaitTerminal(t.Context(), meta.ID, time.Now().Add(15*time.Second), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateDone {
		t.Fatalf("state = %q terminal = %v, want done/true", st.State, terminal)
	}
	// Well under captureGraceWindow (5s): a disabled capture owes no grace.
	if elapsed > 2*time.Second {
		t.Fatalf("WaitTerminal took %s for a capture-disabled job; grace must be skipped", elapsed)
	}
}
