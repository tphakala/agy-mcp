package manager

import (
	"os/exec"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// startSleeper spawns a long-running process a liveness test can inspect and then
// kill. ping to the loopback is available on every Windows host.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestReadStartTimeTicksNonZero(t *testing.T) {
	cmd := startSleeper(t)
	ticks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok || ticks == 0 {
		t.Fatalf("readStartTimeTicks = (%d, %v), want a non-zero creation time", ticks, ok)
	}
}

func TestProcessAliveLiveThenDead(t *testing.T) {
	// maxConcurrency: 1 matches the old New(config.Config{StateDir: t.TempDir()})
	// zero-value default (newGate(0) clamps to 1), which newManager's own default
	// (4) would otherwise silently diverge from. Inert either way since this test
	// only calls processAlive, never the gate, but pinned for defense in depth.
	m := newManager(t, managerOpts{maxConcurrency: 1})
	cmd := startSleeper(t)
	ticks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok {
		t.Fatal("could not read creation time for the live process")
	}
	meta := jobstore.Meta{ID: "j", PID: cmd.Process.Pid, StartTimeTicks: ticks, BootID: readBootID()}
	if !m.processAlive(meta) {
		t.Fatal("processAlive must be true for a running process with a matching creation time")
	}
	// Kill it and confirm liveness flips to false.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		return !m.processAlive(meta)
	}, "processAlive must be false once the process has exited")
}

func TestProcessAliveRejectsCreationTimeMismatch(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir()})
	cmd := startSleeper(t)
	ticks, ok := readStartTimeTicks(cmd.Process.Pid)
	if !ok {
		t.Fatal("could not read creation time")
	}
	// Same live PID but a creation time that does not match: this models a PID
	// recycled to a different process, which must never read as our supervisor.
	meta := jobstore.Meta{ID: "j", PID: cmd.Process.Pid, StartTimeTicks: ticks + 1, BootID: readBootID()}
	if m.processAlive(meta) {
		t.Fatal("processAlive must reject a matching PID with a mismatched creation time")
	}
}

func TestProcessAliveRejectsNonPositivePID(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir()})
	if m.processAlive(jobstore.Meta{ID: "j", PID: 0}) {
		t.Fatal("processAlive must be false for a non-positive PID")
	}
}

// TestProcessAliveImageNameFallback: with no recorded creation time (an older meta
// or a transient read failure) liveness falls back to matching the image name
// against the configured supervisor executable.
func TestProcessAliveImageNameFallback(t *testing.T) {
	cmd := startSleeper(t) // image name is ping.exe

	match := New(config.Config{StateDir: t.TempDir(), SupervisorExe: "ping.exe"})
	meta := jobstore.Meta{ID: "j", PID: cmd.Process.Pid, StartTimeTicks: 0, BootID: readBootID()}
	if !match.processAlive(meta) {
		t.Fatal("processAlive must match a live process by image name when no ticks are recorded")
	}

	mismatch := New(config.Config{StateDir: t.TempDir(), SupervisorExe: "not-ping.exe"})
	if mismatch.processAlive(meta) {
		t.Fatal("processAlive must reject a process whose image name differs from the supervisor exe")
	}
}
