//go:build linux || darwin

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// waitManager builds a Manager whose jobs run the given fake agy under a fake
// supervisor that writes the same job-dir files the real supervisor derives from
// agy's stream (out, progress.json, result.json). The cache file is an empty
// object so continue_latest never reads the host's real agy cache.
func waitManager(t *testing.T, fake testutil.FakeAgy) *Manager {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy, Agy: &fake})
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
	testutil.WaitFor(t, 3*time.Second, func() bool {
		st, err := m.Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != StateRunning && st.State != StateDone {
			t.Fatalf("job reached %q, want running or done", st.State)
		}
		return st.State == StateDone
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

// A fresh run's conversation id comes back with its terminal status, sourced
// from the result payload the supervisor wrote. There is no capture step and no
// grace window: by the time the job is terminal the id is already on disk.
func TestWaitTerminalReturnsConversationIDFromResult(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "OK", Exit: 0, ConversationID: "abcdabcd-1111-2222-3333-444455556666"}
	m := waitManager(t, fake)
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
	if st.ConversationID != fake.ConvID() {
		t.Fatalf("conversation_id = %q, want %q", st.ConversationID, fake.ConvID())
	}
	if st.Partial {
		t.Fatal("a run that emitted a terminal result must not be reported as partial")
	}
	if st.NumTurns != 1 || st.Usage == nil || st.Usage.TotalTokens != 15 {
		t.Fatalf("accounting not surfaced: num_turns = %d usage = %+v", st.NumTurns, st.Usage)
	}
}

// The conversation id is readable while the job is still running, which is the
// point of reading it from agy's init event instead of inferring it afterwards.
func TestStatusReportsConversationIDWhileRunning(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, ConversationID: "99999999-8888-7777-6666-555544443333"}
	m := waitManager(t, fake)
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// Cancel signals the supervisor pid only, and the fake supervisor does not
	// forward signals, so the sleeping fake agy would outlive the test and race
	// t.TempDir teardown. Kill the whole group, as the sibling fixture in
	// job_posix_test.go does.
	t.Cleanup(func() {
		if meta, lerr := m.store.Load(job.ID); lerr == nil && meta.PID > 0 {
			_ = syscall.Kill(-meta.PID, syscall.SIGKILL)
		}
	})

	testutil.WaitFor(t, 3*time.Second, func() bool {
		st, err := m.Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return st.State == StateRunning && st.ConversationID == fake.ConvID()
	}, "a running job never reported its conversation id")
}

// A run cut short before agy emitted its terminal result reports the text that
// did stream, flagged partial, rather than an empty or falsely complete answer.
func TestStatusPartialWhenNoTerminalResult(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "half an answer", Exit: 0, OmitResult: true}
	m := waitManager(t, fake)
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
	if !st.Partial {
		t.Fatal("a run with no terminal result event must be reported as partial")
	}
	if st.Result != "half an answer" {
		t.Fatalf("result = %q, want the streamed text", st.Result)
	}
	// The id still came through, from the progress file written mid-stream.
	if st.ConversationID != fake.ConvID() {
		t.Fatalf("conversation_id = %q, want %q", st.ConversationID, fake.ConvID())
	}
}

// agy reporting an in-band failure (status ERROR) is a failed job even though it
// exited cleanly, and its own message is what gets reported.
func TestStatusFailsOnInBandError(t *testing.T) {
	fake := testutil.FakeAgy{
		Exit:        0,
		Status:      "ERROR",
		ResultError: "timeout waiting for response",
	}
	m := waitManager(t, fake)
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	st, terminal, err := m.WaitTerminal(t.Context(), job.ID, time.Now().Add(15*time.Second), nil)
	if err != nil {
		t.Fatalf("WaitTerminal: %v", err)
	}
	if !terminal || st.State != StateFailed {
		t.Fatalf("state = %q terminal = %v, want failed/true", st.State, terminal)
	}
	if st.Error != "timeout waiting for response" {
		t.Fatalf("error = %q, want agy's own message", st.Error)
	}
}

// StartJob returning a fresh run's conversation id is the headline behaviour of
// the change, and every other test forces its budget to zero, so gutting
// awaitConversationID to return "" left the suite green. Drive it with a real
// budget instead.
func TestStartJobReturnsFreshConversationIDWithinBudget(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, ConversationID: "77777777-6666-5555-4444-333322221111"}
	m := waitManager(t, fake)
	m.conversationIDWait = 5 * time.Second // the production behaviour, not the zeroed test default

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	t.Cleanup(func() {
		if meta, lerr := m.store.Load(job.ID); lerr == nil && meta.PID > 0 {
			_ = syscall.Kill(-meta.PID, syscall.SIGKILL)
		}
	})
	if job.ConversationID != fake.ConvID() {
		t.Fatalf("StartJob conversation_id = %q, want %q from agy's init event", job.ConversationID, fake.ConvID())
	}
}

// A run that is already over when the wait starts must return at once rather
// than sleeping out the whole budget for an id that is never coming.
func TestStartJobConversationWaitStopsAtTerminalJob(t *testing.T) {
	// No Agy config, so the fake supervisor writes no progress file at all, and
	// it exits immediately.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Exit: 0})
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"})
	m := newManager(t, managerOpts{agyPath: agy, supervisorExe: sup, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	m.conversationIDWait = 30 * time.Second // long enough that sleeping it out would fail the test

	start := time.Now()
	if _, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("StartJob took %s; a terminal job must end the conversation-id wait early", elapsed)
	}
}
