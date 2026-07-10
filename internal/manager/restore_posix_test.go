//go:build linux || darwin

package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// startFakeLiveSupervisor starts a real, long-lived process to stand in for a
// detached job supervisor that survived a manager restart. It returns the pid and
// the resolved path to the binary. Set the Manager's SupervisorExe to that path
// for the Linux comm fallback; on darwin (and whenever a start time is recorded)
// processAlive pins identity by start time and does not consult comm.
func startFakeLiveSupervisor(t *testing.T) (pid int, exePath string) {
	t.Helper()
	exePath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := exec.Command(exePath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake supervisor: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid, exePath
}

// createLiveJob is shared by both "live" and "dead" restore tests: it records
// pid's real start time when readable (a genuinely live pid) and otherwise
// leaves StartTimeTicks at its zero value (a killed-and-reaped pid, whose
// start time can no longer be read) — darwin's processAlive fails closed on a
// live pid with no recorded start time, but a dead pid still reads as dead on
// both platforms via kill(pid,0) regardless of StartTimeTicks.
func createLiveJob(t *testing.T, m *Manager, id, cwd string, pid int) {
	t.Helper()
	meta := jobstore.Meta{
		ID:        id,
		Cwd:       cwd,
		PID:       pid,
		BootID:    readBootID(),
		StartedAt: time.Now(),
	}
	if ticks, ok := readStartTimeTicks(pid); ok {
		meta.StartTimeTicks = ticks
	}
	if _, err := m.store.Create(meta); err != nil {
		t.Fatalf("create live job: %v", err)
	}
}

// A live job whose supervisor survived a manager restart must, after RestoreGate,
// re-occupy its serialization key so a conflicting new run is blocked.
func TestRestoreGateBlocksConflictingRun(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	cwd := t.TempDir()
	createLiveJob(t, m, "live-1", cwd, pid)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}

	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd}); err == nil {
		t.Fatal("a same-cwd run should be blocked by the restored live job's key")
	}
}

// A restored live job must also count against the global concurrency cap, so a
// non-conflicting run is refused once the cap is full.
func TestRestoreGateCountsAgainstCap(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 1, withCacheFile: true})
	createLiveJob(t, m, "live-1", t.TempDir(), pid)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}

	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: t.TempDir()}); err == nil {
		t.Fatal("a run in a different cwd should be refused: the restored job fills the cap")
	}
}

// Once a restored job's detached supervisor exits, the watcher must release its
// gate key so a subsequent same-cwd run can proceed (the key must not leak).
func TestRestoreGateReleasesKeyWhenSupervisorExits(t *testing.T) {
	exePath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := exec.Command(exePath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake supervisor: %v", err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	m.restoredPollInterval = 10 * time.Millisecond
	cwd := t.TempDir()
	createLiveJob(t, m, "live-1", cwd, cmd.Process.Pid)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}

	// While the supervisor is alive the key is held: a same-cwd run is blocked.
	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd}); err == nil {
		t.Fatal("expected the restored live job to block a same-cwd run")
	}

	// The supervisor exits; reap it so processAlive turns false.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	reaped = true

	// The watcher must release the key; a same-cwd run then succeeds.
	testutil.WaitFor(t, 2*time.Second, func() bool {
		_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
		return err == nil
	}, "watcher did not release the restored key after the supervisor exited")
}

// RestoreGate must fail closed: if the on-disk jobs cannot be scanned, it returns
// an error so startup can refuse rather than serve with an unrestored gate.
func TestRestoreGateFailsClosedOnScanError(t *testing.T) {
	state := t.TempDir()
	// Make the jobs path a file so the directory scan errors (distinct from the
	// benign "directory does not exist yet" case, which is not an error).
	if err := os.WriteFile(filepath.Join(state, "jobs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: "/bin/true", stateDir: state, defaultTimeout: time.Minute, maxConcurrency: 4})
	if err := m.RestoreGate(); err == nil {
		t.Fatal("RestoreGate must return an error when the jobs dir cannot be scanned")
	}
}

// A job whose recorded supervisor is no longer alive must NOT be restored into the
// gate. Restoring a dead job would hold its key forever and block all same-cwd runs.
func TestRestoreGateSkipsDeadSupervisor(t *testing.T) {
	exePath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	// Start then immediately kill+reap a real process so its pid is dead (and from
	// the current boot, so only the liveness check, not the boot-id guard, rejects it).
	cmd := exec.Command(exePath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	cwd := t.TempDir()
	createLiveJob(t, m, "dead-1", cwd, deadPID)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}

	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd}); err != nil {
		t.Fatalf("a dead-supervisor job must not hold a gate key, but the run was blocked: %v", err)
	}
}

// A terminal job (one with an exit_code sentinel) must NOT be restored into the
// gate, even if its recorded pid happens to still be alive.
func TestRestoreGateSkipsTerminalJobs(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4, withCacheFile: true})
	cwd := t.TempDir()
	createLiveJob(t, m, "done-1", cwd, pid)
	if err := m.store.WriteExitCode("done-1", 0); err != nil {
		t.Fatal(err)
	}

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}

	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd}); err != nil {
		t.Fatalf("a terminal job must not hold a gate key, but the run was blocked: %v", err)
	}
}

// RestoreAndCollect must do both halves in a single startup pass: garbage-collect
// an expired, finished job while re-occupying the gate for a live job whose
// supervisor outlived the restart. The live job is itself past the TTL, so this
// also confirms the fused pass keeps (never collects) a still-alive job, and that
// removing one job does not disturb restoring another.
func TestRestoreAndCollectCollectsExpiredAndRestoresLive(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4, jobTTL: time.Hour, withCacheFile: true})

	// An expired, finished job: past the TTL with an exit sentinel, so GC removes it.
	if _, err := m.store.Create(jobstore.Meta{ID: "old-done", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("old-done", 0); err != nil {
		t.Fatal(err)
	}
	// A live job whose supervisor survived the restart, itself past the TTL: GC must
	// keep it (alive), and the gate must re-occupy its key.
	liveCwd := t.TempDir()
	startTicks, ok := readStartTimeTicks(pid)
	if !ok {
		t.Fatal("readStartTimeTicks(target) failed")
	}
	if _, err := m.store.Create(jobstore.Meta{
		ID:             "live-1",
		Cwd:            liveCwd,
		PID:            pid,
		BootID:         readBootID(),
		StartTimeTicks: startTicks,
		StartedAt:      time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := m.RestoreAndCollect()
	if err != nil {
		t.Fatalf("RestoreAndCollect: %v", err)
	}
	if len(removed) != 1 || removed[0] != "old-done" {
		t.Fatalf("removed = %v, want [old-done]", removed)
	}
	if _, err := m.store.Load("old-done"); err == nil {
		t.Fatal("the expired finished job should have been removed")
	}
	if _, err := m.store.Load("live-1"); err != nil {
		t.Fatalf("the expired-but-alive job must be kept: %v", err)
	}
	// The live job's gate key is held, so a same-cwd run is blocked.
	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: liveCwd}); err == nil {
		t.Fatal("the restored live job must block a same-cwd run")
	}
}

// A restored fresh run must have its conversation id captured by the watcher
// when its supervisor finishes, exactly like the StartJob completion path, and
// the id must land on disk without any Status call (the watcher, not a poller,
// owns the capture while the gate key is held).
func TestRestoreGateCapturesConversationIDOnExit(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4})
	m.restoredPollInterval = 20 * time.Millisecond

	cwd := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.cacheFile = cachePath

	createLiveJob(t, m, "restored-fresh", cwd, pid)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}
	if !m.CapturePending("restored-fresh") {
		t.Fatal("a restored fresh run must have its capture armed")
	}

	// The job "finishes": agy's cache gains the new conversation, then the
	// supervisor records exit 0 (the watcher treats the sentinel as terminal
	// even while the fake supervisor process lingers).
	const uuid = "deadbeef-1111-2222-3333-444455556666"
	if err := os.WriteFile(cachePath, fmt.Appendf(nil, `{%q:%q}`, cwd, uuid), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("restored-fresh", 0); err != nil {
		t.Fatal(err)
	}

	testutil.WaitFor(t, 3*time.Second, func() bool {
		meta, err := m.store.Load("restored-fresh")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return meta.ConversationID == uuid
	}, "watcher never captured the conversation id")
}
