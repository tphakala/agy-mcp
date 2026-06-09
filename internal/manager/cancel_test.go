package manager

import (
	"os"
	execpkg "os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

type execCmd = execpkg.Cmd

func newExecCmd(name string, args ...string) *execCmd { return execpkg.Command(name, args...) }

func exec_Command_sleep(t *testing.T) *execCmd {
	t.Helper()
	return newExecCmd("sleep", "30")
}

func TestCancelSignalsSupervisor(t *testing.T) {
	m := newTestManager(t)
	// Spawn a real sleeper process we can signal.
	cmd := exec_Command_sleep(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	dir, _ := m.store.Create(jobstore.Meta{
		ID: "j", PID: cmd.Process.Pid, BootID: readBootID(), StartedAt: time.Now(),
	})
	_ = dir

	if err := m.Cancel("j"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// The sleeper should receive SIGTERM and exit; wait for it.
	_ = cmd.Wait()

	// Simulate the supervisor's sentinel write that Cancel triggers in production.
	_ = m.store.WriteExitCode("j", 143)
	st, _ := m.Status("j")
	if st.State != "cancelled" {
		t.Fatalf("state = %q, want cancelled", st.State)
	}
	_ = os.Remove(filepath.Join(m.store.Dir("j"), "out"))
	_ = syscall.Kill // keep import
}
