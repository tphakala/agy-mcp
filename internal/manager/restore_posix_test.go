//go:build linux || darwin

package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
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

// liveConvID is the conversation a createLiveJob job is running, derived from
// its id. Restored jobs are given one because a conversation is the only thing
// that still keys the gate: a fresh run keys on nothing, so a restored fresh run
// would block nothing and could not exercise key restoration at all.
func liveConvID(id string) string { return "conv-" + id }

// createLiveJob is shared by both "live" and "dead" restore tests: it records
// pid's real start time when readable (a genuinely live pid) and otherwise
// leaves StartTimeTicks at its zero value (a killed-and-reaped pid, whose
// start time can no longer be read). darwin's processAlive fails closed on a
// live pid with no recorded start time, but a dead pid still reads as dead on
// both platforms via kill(pid,0) regardless of StartTimeTicks.
func createLiveJob(t *testing.T, m *Manager, id, cwd string, pid int) {
	t.Helper()
	meta := jobstore.Meta{
		ID:             id,
		Cwd:            cwd,
		ConversationID: liveConvID(id),
		PID:            pid,
		BootID:         readBootID(),
		StartedAt:      time.Now(),
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

	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd, ConversationID: liveConvID("live-1")}); err == nil {
		t.Fatal("a run continuing the same conversation should be blocked by the restored live job's key")
	}
	// Two fresh runs in that same directory must BOTH admit: fresh runs no longer
	// key on the cwd, so nothing serializes them against the restored job or
	// against each other. A single fresh run admitting proved nothing here, since a
	// fresh run keys on "" and can never collide with the restored conv: key;
	// running two in one directory is what pins that they are not serialized. If
	// keyFor were changed back to keying fresh runs on the cwd, the second run
	// would be refused with acquireKeyBusy and this loop would fail.
	for i := range 2 {
		if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd}); err != nil {
			t.Fatalf("fresh same-cwd run %d must not be blocked by a restored job or a sibling fresh run: %v", i+1, err)
		}
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

	// While the supervisor is alive the key is held: a run continuing the same
	// conversation is blocked.
	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd, ConversationID: liveConvID("live-1")}); err == nil {
		t.Fatal("expected the restored live job to block a run on the same conversation")
	}

	// The supervisor exits; reap it so processAlive turns false.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	reaped = true

	// The watcher must release the key; a same-cwd run then succeeds. Any error
	// other than a known retryable gate refusal is a genuine bug, not something
	// to retry through.
	testutil.WaitFor(t, 2*time.Second, func() bool {
		_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
		if err == nil {
			return true
		}
		if !isRetryableGateRefusal(err) {
			t.Fatalf("unexpected error: %v", err)
		}
		return false
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
		ConversationID: "live-conv",
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
	// The live job's gate key is held, so a run continuing its conversation is
	// blocked.
	if _, err := m.StartJob(StartRequest{Prompt: "x", Cwd: liveCwd, ConversationID: "live-conv"}); err == nil {
		t.Fatal("the restored live job must block a run on its conversation")
	}
}

// A restored job reports the conversation id the supervisor recorded in its
// progress file, with no capture step and no Status-driven polling: the id is
// already on disk by the time the manager restarts.
func TestRestoreGateReportsSupervisorRecordedConversationID(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManager(t, managerOpts{agyPath: "/usr/bin/agy", supervisorExe: exePath, defaultTimeout: time.Minute, maxConcurrency: 4})
	m.restoredPollInterval = 20 * time.Millisecond

	cwd := t.TempDir()
	// A fresh run: meta carries no conversation id, so the only source is the
	// progress file the supervisor wrote when agy's init event arrived.
	meta := jobstore.Meta{ID: "restored-fresh", Cwd: cwd, PID: pid, BootID: readBootID(), StartedAt: time.Now()}
	if ticks, ok := readStartTimeTicks(pid); ok {
		meta.StartTimeTicks = ticks
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatalf("create live job: %v", err)
	}

	const uuid = "deadbeef-1111-2222-3333-444455556666"
	if err := jobstore.WriteProgressDir(dir, jobstore.Progress{ConversationID: uuid, StepType: "agent_response"}); err != nil {
		t.Fatalf("write progress: %v", err)
	}

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}
	st, err := m.Status("restored-fresh")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ConversationID != uuid {
		t.Fatalf("ConversationID = %q, want %q from the progress file", st.ConversationID, uuid)
	}
	if st.State != StateRunning {
		t.Fatalf("State = %q, want running", st.State)
	}
}
