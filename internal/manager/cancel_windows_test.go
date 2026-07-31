package manager

import (
	"os"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// TestCancelWritesSentinelWhenAlive: on Windows the manager requests a cancel by
// writing the sentinel file the supervisor polls for, so Cancel of a live job
// must create it.
func TestCancelWritesSentinelWhenAlive(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})
	cmd := startSleeper(t) // stands in for a live supervisor
	ticks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok {
		t.Fatal("could not read creation time for the live process")
	}
	dir, err := m.store.Create(jobstore.Meta{
		ID: "j", PID: cmd.Process.Pid, StartTimeTicks: ticks, BootID: readBootID(), StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("j"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(jobstore.CancelPath(dir)); err != nil {
		t.Fatalf("cancel sentinel not written: %v", err)
	}
}

// TestCancelDeadSupervisorNoOp: when the recorded supervisor is no longer alive,
// Cancel is a no-op success and must not write a sentinel (there is nothing to
// cancel; Status reports the terminal state from disk).
func TestCancelDeadSupervisorNoOp(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})
	dir, err := m.store.Create(jobstore.Meta{
		ID: "j", PID: 999999, StartTimeTicks: 12345, BootID: readBootID(), StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("j"); err != nil {
		t.Fatalf("Cancel of a dead supervisor must be a no-op success, got %v", err)
	}
	if _, err := os.Stat(jobstore.CancelPath(dir)); err == nil {
		t.Fatal("Cancel must not write a sentinel for a dead supervisor")
	}
}
