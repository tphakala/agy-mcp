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

// A job that is already done when WaitTerminal is called must return its
// terminal status without waiting.
func TestWaitTerminalReturnsDoneJob(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// 5s, the package's usual bound for this shape. It waits on a spawned supervisor
	// and a forked fake agy, so the deadline is bounded by machine load rather than
	// by anything under test, and at 3s it flaked on a loaded full-suite run.
	testutil.WaitFor(t, 5*time.Second, func() bool {
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
	// Kill the job's process group at cleanup instead of draining it to
	// completion. The fake sleeps 2s, and polling for that under a loaded -race
	// run raced the old 5s drain budget (issue #163). killJob only registers a
	// t.Cleanup, so the job keeps running through the deadline assertion below; by
	// the time that cleanup fires the still-running job only needs to stop writing
	// into the TempDir state dir before it is removed, which SIGKILL does.
	killJob(t, m, job.ID)
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
}

// A cancelled context must end the wait with context.Canceled and a
// non-terminal result, without touching the still-running job.
func TestWaitTerminalContextCancel(t *testing.T) {
	m := waitManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 2 * time.Second})
	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// Kill the job's process group at cleanup rather than draining it. The fake
	// sleeps 2s, and polling for that under a loaded -race run raced the old 5s
	// drain budget (issue #163). The cancel assertion below is what the test is
	// about; the still-running job only needs to stop writing into the TempDir
	// state dir before it is removed, which SIGKILL does.
	killJob(t, m, job.ID)
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
	killJob(t, m, job.ID)

	testutil.WaitFor(t, 5*time.Second, func() bool {
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

// killJob arranges for a still-running job's process group to be SIGKILLed when
// the test ends, so a fake agy does not outlive its test and keep writing into a
// TempDir state dir that is being removed. Cancel is not an alternative: it
// signals the supervisor pid alone and the fake supervisor forwards nothing, so
// the fake agy it runs in the foreground survives.
//
// The PID guard is load-bearing rather than defensive: syscall.Kill(-0, ...)
// would signal the test runner's own process group. The Kill error is discarded
// because ESRCH, for a group that has already exited, is the ordinary case; the
// pid is read from disk, so a recycled one is possible in principle and unguarded
// here, which is tolerable only because this is test-only cleanup.
//
// Like the inline cleanups it replaced, it signals and returns without waiting for
// the group to die, which is enough for the purpose.
func killJob(t *testing.T, m *Manager, id string) {
	t.Helper()
	t.Cleanup(func() {
		if meta, err := m.store.Load(id); err == nil && meta.PID > 0 {
			_ = syscall.Kill(-meta.PID, syscall.SIGKILL)
		}
	})
}

// StartJob must not resolve a conversation id. It used to, and agy_run_sync
// discarded the result. Resolving it is AwaitConversationID's job now.
//
// The fixture is what makes this falsifiable: the fake supervisor stages an id in
// progress.json before agy starts, so a StartJob that still waited would find one
// and hand it back, and the empty value is the proof the wait is gone. An
// id-less fixture would report empty either way.
// TestAgyRunSyncDoesNotPayTheConversationIDBudget covers the latency half.
func TestStartJobDoesNotResolveAConversationID(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, ConversationID: "88888888-7777-6666-5555-444433332222"}
	m := waitManager(t, fake)
	m.conversationIDWait = 30 * time.Second // a wait left in StartJob would have room to find the id

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	killJob(t, m, job.ID)
	if job.ConversationID != "" {
		t.Fatalf("StartJob conversation_id = %q, want empty: a fresh run is named by AwaitConversationID, not here", job.ConversationID)
	}
}

// The budget has to actually expire. Moving the wait out of StartJob cost this
// branch the coverage it had by accident, since every fresh-run test used to
// traverse it; without this test, neutering the deadline check leaves both
// packages green. What it would break is agy_run, whose whole contract is to
// return promptly: a wait that cannot expire blocks it for the length of the run.
func TestAwaitConversationIDGivesUpWhenTheBudgetExpires(t *testing.T) {
	// Never names a conversation and outlives the budget many times over, so
	// neither early exit can fire and only the deadline can end the wait.
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, NoConversationID: true}
	m := waitManager(t, fake)
	const budget = 300 * time.Millisecond
	m.conversationIDWait = budget

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	killJob(t, m, job.ID)
	start := time.Now()
	got := m.AwaitConversationID(t.Context(), job)
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("AwaitConversationID = %q, want empty: this run never names a conversation", got)
	}
	// Both bounds are load-bearing, and neither alone pins the deadline.
	//
	// The lower one separates "the budget ran out" from "an early exit fired",
	// since only the former reaches the deadline at all.
	//
	// The upper one is what catches a deadline that never fires. Without it the
	// wait simply runs on until the fake agy exits and the terminal-job branch
	// ends it, returning the same empty id after ~30s, which clears the lower
	// bound trivially and passes. Measured that way: 30.55s.
	if elapsed < budget {
		t.Fatalf("AwaitConversationID returned in %s, inside the %s budget: an early exit fired, so the deadline was never reached", elapsed, budget)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("AwaitConversationID took %s against a %s budget: the deadline never fired and some later exit ended the wait", elapsed, budget)
	}
}

// A job id the store refuses resolves to no conversation, rather than spending
// the budget polling the empty path Dir returns alongside its error. This is the
// only test that reaches that branch: the continuation test passes an id this
// manager never issued too, but short-circuits on the id it carries before Dir
// is ever called.
func TestAwaitConversationIDRejectsAnUnusableJobID(t *testing.T) {
	m := newManager(t, managerOpts{})
	m.conversationIDWait = 30 * time.Second

	start := time.Now()
	got := m.AwaitConversationID(t.Context(), Job{ID: "../escape"})
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("AwaitConversationID = %q, want empty for a job id the store rejects", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("AwaitConversationID polled for %s on a job id the store rejects", elapsed)
	}
}

// AwaitConversationID returning a fresh run's conversation id is the headline
// behaviour of the stream-json change, and the shared test manager leaves its
// budget at zero, so gutting it to return "" left the suite green. Drive it with
// a real budget instead.
func TestAwaitConversationIDReturnsFreshIDWithinBudget(t *testing.T) {
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, ConversationID: "77777777-6666-5555-4444-333322221111"}
	m := waitManager(t, fake)
	m.conversationIDWait = 5 * time.Second // a real budget, not the zero the shared helper leaves

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	killJob(t, m, job.ID)
	if got := m.AwaitConversationID(t.Context(), job); got != fake.ConvID() {
		t.Fatalf("AwaitConversationID = %q, want %q from agy's init event", got, fake.ConvID())
	}
}

// A run that is already over when the wait starts must return at once rather
// than sleeping out the whole budget for an id that is never coming.
func TestAwaitConversationIDStopsAtTerminalJob(t *testing.T) {
	// No Agy config, so the only progress file the fake supervisor stages is the
	// marker, which carries no conversation id, and it exits immediately.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Exit: 0})
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"})
	m := newManager(t, managerOpts{agyPath: agy, supervisorExe: sup, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	m.conversationIDWait = 30 * time.Second // long enough that sleeping it out would fail the test

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	start := time.Now()
	got := m.AwaitConversationID(t.Context(), job)
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("AwaitConversationID = %q, want empty: this run reports no id", got)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("AwaitConversationID took %s; a terminal job must end the wait early", elapsed)
	}
}

// A continuation already carries the id, so the wait must never start: polling
// for a value that is already in hand would spend the budget on a job dir that
// may not even be readable yet.
func TestAwaitConversationIDShortCircuitsAContinuation(t *testing.T) {
	// No agy and no supervisor: this test starts no job, it only asks about one.
	m := newManager(t, managerOpts{})
	m.conversationIDWait = 30 * time.Second

	const convID = "11111111-2222-3333-4444-555555555555"
	start := time.Now()
	// No such job dir, so a wait that did start would poll out its whole budget.
	got := m.AwaitConversationID(t.Context(), Job{ID: "no-such-job", ConversationID: convID})
	elapsed := time.Since(start)
	if got != convID {
		t.Fatalf("AwaitConversationID = %q, want %q handed straight back", got, convID)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("AwaitConversationID took %s for an id it was given", elapsed)
	}
}

// A cancelled caller context must end the wait at once rather than polling out
// the budget. The agy_run handler holds a live context; when the client abandons
// the call there is no reason to keep parking it, and the id it would have
// reported is delivered by the next agy_status read instead, so an early "" loses
// nothing. Without the ctx.Done() case this run would poll the full budget.
func TestAwaitConversationIDStopsOnContextCancellation(t *testing.T) {
	// Never names a conversation and outlives the budget many times over, so
	// neither the id-found nor the terminal branch can fire; only cancellation can
	// end the wait.
	fake := testutil.FakeAgy{Stdout: "OK", Sleep: 30 * time.Second, NoConversationID: true}
	m := waitManager(t, fake)
	m.conversationIDWait = 30 * time.Second // long enough that sleeping it out would fail the test

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	killJob(t, m, job.ID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled: the wait must return at its first poll boundary

	start := time.Now()
	got := m.AwaitConversationID(ctx, job)
	elapsed := time.Since(start)
	if got != "" {
		t.Fatalf("AwaitConversationID = %q, want empty when the caller's context is cancelled", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("AwaitConversationID took %s against a %s budget: cancellation did not end the wait", elapsed, m.conversationIDWait)
	}
}
