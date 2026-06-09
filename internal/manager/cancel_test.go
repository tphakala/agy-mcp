package manager

import (
	execpkg "os/exec"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

func TestCancelSignalsSupervisor(t *testing.T) {
	// The liveness guard requires the target's /proc comm to match the
	// configured supervisor basename, so stand in "sleep" as the supervisor.
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, SupervisorExe: "sleep"})

	// Spawn a real sleeper process we can signal.
	cmd := execpkg.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	if _, err := m.store.Create(jobstore.Meta{
		ID: "j", PID: cmd.Process.Pid, BootID: readBootID(), StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.Cancel("j"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancel must have actually signaled the sleeper, which then exits non-zero.
	if err := cmd.Wait(); err == nil {
		t.Fatal("Cancel did not terminate the target process")
	}

	// In production the supervisor writes the sentinel on SIGTERM; simulate that
	// here. This test exercises the manager's signal-and-status path; the
	// supervisor binary's own terminate-and-sentinel logic is covered by the
	// supervisor tests.
	_ = m.store.WriteExitCode("j", jobstore.ExitSIGTERM)
	st, _ := m.Status("j")
	if st.State != StateCancelled {
		t.Fatalf("state = %q, want %q", st.State, StateCancelled)
	}
}
