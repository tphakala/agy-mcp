//go:build darwin

package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/testutil"
)

// TestStartJobAbortsSpawnOnDarwinStartTimeFailure exercises the darwin-only
// fail-closed branch in StartJob: when readStartTimeTicks cannot read the
// just-spawned supervisor's start time, startTimeMandatory (true on darwin)
// makes StartJob tear the supervisor down via abortSpawn and return an error,
// rather than persist StartTimeTicks==0 (which processAlive treats as dead on
// darwin, since there is no /proc/comm fallback).
//
// The real trigger (a sysctl kern.proc.pid read of a child we still hold, now
// retried) is essentially unreachable in practice, so this forces it via the
// readStartTimeTicksFn seam instead of trying to reproduce a genuine sysctl
// failure. abortSpawn's teardown sequence itself is exercised by proxy through
// TestStartJobCleansUpDirOnUpdateMetaFailure (cleanup_posix_test.go, via the
// failUpdateStore seam); this test is the direct trigger for the darwin path.
func TestStartJobAbortsSpawnOnDarwinStartTimeFailure(t *testing.T) {
	orig := readStartTimeTicksFn
	readStartTimeTicksFn = func(int) (uint64, bool) { return 0, false }
	t.Cleanup(func() { readStartTimeTicksFn = orig })

	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 1,
	})
	cwd := t.TempDir()

	_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "record supervisor start time") {
		t.Fatalf("StartJob error = %v, want a record-supervisor-start-time failure", err)
	}

	// abortSpawn tears the supervisor down asynchronously (after cmd.Wait), so
	// the job dir removal is not immediate; poll with a bounded deadline.
	testutil.WaitFor(t, 5*time.Second, func() bool {
		ids, lerr := m.store.List()
		if lerr != nil {
			t.Fatal(lerr)
		}
		return len(ids) == 0
	}, "job dir not removed after darwin start-time-failure abort")

	// The gate slot/key must also be released: a second same-cwd run must get
	// PAST the gate (and hit the same start-time failure again) rather than
	// being refused as a conflicting job. With MaxConcurrency 1, a leaked slot
	// would also block it, so reaching the spawn proves both freed.
	testutil.WaitFor(t, 5*time.Second, func() bool {
		_, err2 := m.StartJob(StartRequest{Prompt: "again", Cwd: cwd})
		switch {
		case err2 != nil && strings.Contains(err2.Error(), "record supervisor start time"):
			return true
		case err2 != nil && strings.Contains(err2.Error(), "conflicting"):
			return false
		default:
			t.Fatalf("unexpected second-run error: %v", err2)
			return false
		}
	}, "gate slot/key not released after darwin start-time-failure abort")

	// That second run also launched an async cleanup goroutine; wait for the
	// store to drain before returning so its supervisor and dir removal don't
	// race t.TempDir teardown.
	waitForEmptyStore(t, m)
}
