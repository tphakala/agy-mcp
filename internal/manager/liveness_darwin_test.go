//go:build darwin

package manager

import (
	"os"
	"os/exec"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestReadStartTimeTicksSelfAndBogus(t *testing.T) {
	self, ok := readStartTimeTicks(os.Getpid())
	if !ok || self == 0 {
		t.Fatalf("readStartTimeTicks(self) = %d,%v, want non-zero,true", self, ok)
	}
	if v, ok := readStartTimeTicks(1 << 30); ok || v != 0 {
		t.Errorf("readStartTimeTicks(bogus) = %d,%v, want 0,false", v, ok)
	}
}

func TestReadBootIDSentinel(t *testing.T) {
	if got := readBootID(); got != "darwin" {
		t.Errorf("readBootID() = %q, want %q", got, "darwin")
	}
}

func TestProcessAliveDarwin(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	ticks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok {
		t.Fatal("readStartTimeTicks(child) failed")
	}

	// Alive: matching pid + start time.
	if !m.processAlive(jobstore.Meta{PID: cmd.Process.Pid, StartTimeTicks: ticks}) {
		t.Error("processAlive(live pid, matching ticks) = false, want true")
	}
	// Recycled: same pid, different recorded start time -> dead.
	if m.processAlive(jobstore.Meta{PID: cmd.Process.Pid, StartTimeTicks: ticks + 1}) {
		t.Error("processAlive(live pid, mismatched ticks) = true, want false (recycled)")
	}
	// Fail closed: no recorded start time -> dead (no name fallback on darwin).
	if m.processAlive(jobstore.Meta{PID: cmd.Process.Pid, StartTimeTicks: 0}) {
		t.Error("processAlive(live pid, StartTimeTicks=0) = true, want false (fail closed)")
	}
	// Non-existent pid -> dead.
	if m.processAlive(jobstore.Meta{PID: 1 << 30, StartTimeTicks: ticks}) {
		t.Error("processAlive(bogus pid) = true, want false")
	}
	// Non-positive pid -> dead.
	if m.processAlive(jobstore.Meta{PID: 0, StartTimeTicks: ticks}) {
		t.Error("processAlive(pid 0) = true, want false")
	}
}
