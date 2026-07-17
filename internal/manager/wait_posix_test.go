//go:build linux || darwin

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

	st, terminal, err := m.WaitTerminal(context.Background(), job.ID, time.Now().Add(5*time.Second), nil)
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
	st, terminal, err := m.WaitTerminal(context.Background(), job.ID, time.Now().Add(15*time.Second), nil)
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
	st, terminal, err := m.WaitTerminal(context.Background(), job.ID, time.Now().Add(100*time.Millisecond), nil)
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
	ctx, cancel := context.WithCancel(context.Background())
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
	_, terminal, err := m.WaitTerminal(context.Background(), "nonexistent-id", time.Now().Add(5*time.Second), nil)
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
	st, terminal, err := m.WaitTerminal(context.Background(), job.ID, time.Now().Add(15*time.Second), onTick)
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
