package manager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// failUpdateStore wraps a real store but fails UpdateMeta whenever failUpdateMeta is
// set. StartJob calls UpdateMeta exactly once on the success path, right after the
// supervisor spawns, so injecting a failure there drives StartJob's post-spawn cleanup
// path (terminate the group, wait, remove the dir, release the gate) - issue #12.
type failUpdateStore struct {
	jobStore       // the real *jobstore.Store, via the interface
	failUpdateMeta bool
}

func (f *failUpdateStore) UpdateMeta(m jobstore.Meta) error {
	if f.failUpdateMeta {
		return errors.New("injected UpdateMeta failure")
	}
	return f.jobStore.UpdateMeta(m)
}

// TestStartJobCleansUpDirOnUpdateMetaFailure covers the second StartJob failure path:
// Create and cmd.Start succeed (a real supervisor runs), then UpdateMeta fails. The
// manager must terminate the supervisor, wait for it to exit, remove the job dir, and
// release the gate. cleanup runs in a goroutine after cmd.Wait, so the assertions poll.
func TestStartJobCleansUpDirOnUpdateMetaFailure(t *testing.T) {
	state := t.TempDir()
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  fakeSupervisor(t), // real: cmd.Start succeeds and a supervisor runs
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 1,
	}
	m := New(c)
	m.store = &failUpdateStore{jobStore: m.store, failUpdateMeta: true}
	cwd := t.TempDir()

	_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "record supervisor pid") {
		t.Fatalf("StartJob error = %v, want a record-supervisor-pid failure", err)
	}

	// The job dir must be removed (no orphan left for GarbageCollect). cleanup is
	// asynchronous (after cmd.Wait), so poll with a bounded deadline.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, lerr := m.store.List()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(ids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job dir not removed after UpdateMeta-failure cleanup: %v", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The gate slot and key must be released after the supervisor exits: a second
	// same-cwd run must get PAST the gate (it then fails at UpdateMeta again with the
	// same store) rather than being refused as a conflicting job. With MaxConcurrency
	// 1, a leaked slot would also block it, so reaching the spawn proves both freed.
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err2 := m.StartJob(StartRequest{Prompt: "again", Cwd: cwd})
		switch {
		case err2 != nil && strings.Contains(err2.Error(), "record supervisor pid"):
			// Got past the gate, spawned, failed at UpdateMeta again: the gate was
			// released. That second run also launched an async cleanup goroutine; wait
			// for the store to drain before returning so its supervisor and dir removal
			// don't race t.TempDir teardown (the same race the first poll above guards).
			waitForEmptyStore(t, m)
			return
		case err2 != nil && strings.Contains(err2.Error(), "conflicting"):
			if time.Now().After(deadline) {
				t.Fatal("gate slot/key not released after UpdateMeta-failure cleanup")
			}
			time.Sleep(20 * time.Millisecond)
		default:
			t.Fatalf("unexpected second-run error: %v", err2)
		}
	}
}

// waitForEmptyStore blocks until no job dirs remain, i.e. every async cleanup goroutine
// has removed its job dir, so a test can return without a supervisor or remover racing
// t.TempDir teardown.
func waitForEmptyStore(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ids, err := m.store.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store did not drain: %v", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
