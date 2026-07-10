//go:build linux || darwin

package manager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
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
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}), // real: cmd.Start succeeds and a supervisor runs
		defaultTimeout: time.Minute,
		maxConcurrency: 1,
	})
	m.store = &failUpdateStore{jobStore: m.store, failUpdateMeta: true}
	cwd := t.TempDir()

	_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "record supervisor pid") {
		t.Fatalf("StartJob error = %v, want a record-supervisor-pid failure", err)
	}

	// The job dir must be removed (no orphan left for GarbageCollect). cleanup is
	// asynchronous (after cmd.Wait), so poll with a bounded deadline.
	testutil.WaitFor(t, 5*time.Second, func() bool {
		ids, lerr := m.store.List()
		if lerr != nil {
			t.Fatal(lerr)
		}
		return len(ids) == 0
	}, "job dir not removed after UpdateMeta-failure cleanup")

	// The gate slot and key must be released after the supervisor exits: a second
	// same-cwd run must get PAST the gate (it then fails at UpdateMeta again with the
	// same store) rather than being refused as a conflicting job. With MaxConcurrency
	// 1, a leaked slot would also block it, so reaching the spawn proves both freed.
	testutil.WaitFor(t, 5*time.Second, func() bool {
		_, err2 := m.StartJob(StartRequest{Prompt: "again", Cwd: cwd})
		switch {
		case err2 != nil && strings.Contains(err2.Error(), "record supervisor pid"):
			// Got past the gate, spawned, failed at UpdateMeta again: the gate was released.
			return true
		case isRetryableGateRefusal(err2):
			return false
		default:
			t.Fatalf("unexpected second-run error: %v", err2)
			return false
		}
	}, "gate slot/key not released after UpdateMeta-failure cleanup")

	// That second run also launched an async cleanup goroutine; wait for the store to
	// drain before returning so its supervisor and dir removal don't race t.TempDir
	// teardown (the same race the first poll above guards).
	waitForEmptyStore(t, m)
}

// waitForEmptyStore blocks until no job dirs remain, i.e. every async cleanup goroutine
// has removed its job dir, so a test can return without a supervisor or remover racing
// t.TempDir teardown.
func waitForEmptyStore(t *testing.T, m *Manager) {
	t.Helper()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		ids, err := m.store.List()
		if err != nil {
			t.Fatal(err)
		}
		return len(ids) == 0
	}, "store did not drain")
}

// isRetryableGateRefusal reports whether err is one of the known transient
// "another job holds this key, try again" outcomes a same-key StartJob retry
// loop should keep polling through, rather than treating as a hard failure:
//   - "conflicting": admit's acquireKeyBusy branch (manager.go), the common case.
//   - "concurrency cap": admit's acquireAtCap branch, a sibling refusal for the
//     same class of outcome (see concurrency.go's comment on why the two are
//     reported with different text).
//   - "already held by this process": releaseKey releases the in-process gate
//     slot before the cross-process lock (admit.go), so a retry landing in that
//     narrow window can observe the gate as free while xlock.tryLock still hits
//     its own duplicate guard (keylock.go). Tracked for a real fix in #81; this
//     is the test-side tolerance for it in the meantime.
func isRetryableGateRefusal(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "conflicting") ||
		strings.Contains(msg, "concurrency cap") ||
		strings.Contains(msg, "already held by this process")
}
