//go:build linux || darwin

package manager

import (
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// TestGarbageCollectKeepsExpiredButAliveJob pins GC's core safety invariant: a
// job past the TTL whose supervisor is still alive must NOT be reaped, or GC
// would delete a live job dir out from under its running supervisor. Every
// other GC test uses dead metas, so a regression that dropped the liveness
// check would pass them; this one needs a real, live process to catch it.
func TestGarbageCollectKeepsExpiredButAliveJob(t *testing.T) {
	// processAlive pins liveness by the recorded start time (set below), so any
	// real, live process stands in as the supervisor. SupervisorExe feeds only
	// the Linux comm fallback, which a recorded start time bypasses.
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour, SupervisorExe: "sleep"})

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill and reap (Wait) so the sleeper does not linger as a zombie.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Expired (StartedAt well before the TTL cutoff) but its supervisor is alive.
	startTicks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok {
		t.Fatal("readStartTimeTicks(target) failed")
	}
	if _, err := m.store.Create(jobstore.Meta{
		ID:             "alive",
		PID:            cmd.Process.Pid,
		BootID:         readBootID(),
		StartTimeTicks: startTicks,
		StartedAt:      time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(removed, "alive") {
		t.Fatalf("GC removed an expired job whose supervisor is still alive; removed=%v", removed)
	}
	if _, err := m.store.Load("alive"); err != nil {
		t.Fatalf("the live job's dir must still exist after GC: %v", err)
	}
}
