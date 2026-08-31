package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// startSleeper spawns a process that runs for ~60s so a test can terminate it.
// ping to the loopback with a high count is a dependency-free long-running
// process available on every Windows host.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("ping", "-n", "60", "127.0.0.1")
	ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	// Reap the process even if the test fatals before terminating it, so a failed
	// assertion never leaves a 60s ping running.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestConfigureGroupSetsNewProcessGroup(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	ConfigureGroup(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("ConfigureGroup must set CREATE_NEW_PROCESS_GROUP")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("ConfigureGroup must set CREATE_NO_WINDOW")
	}
}

func TestConfigureGroupPreservesExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	// The sentinel has to be a flag ConfigureGroup does not set itself, otherwise
	// the check passes even when CreationFlags is overwritten rather than ORed.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_DEFAULT_ERROR_MODE}
	ConfigureGroup(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_DEFAULT_ERROR_MODE == 0 {
		t.Error("ConfigureGroup must preserve a pre-existing CreationFlag")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("ConfigureGroup must add CREATE_NEW_PROCESS_GROUP")
	}
}

// ConfigureNoWindow is what keeps the manager's short-lived probes from flashing
// a console window when the manager itself has no console to inherit.
func TestConfigureNoWindowSetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	ConfigureNoWindow(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("ConfigureNoWindow must set CREATE_NO_WINDOW")
	}
}

// The probes want window suppression alone, and neither probe is supervised as a
// tree. Asserting the absence keeps ConfigureNoWindow from quietly becoming an
// alias for ConfigureGroup.
func TestConfigureNoWindowDoesNotStartNewProcessGroup(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	ConfigureNoWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("ConfigureNoWindow must set SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0 {
		t.Error("ConfigureNoWindow must not set CREATE_NEW_PROCESS_GROUP")
	}
	if cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Error("ConfigureNoWindow must not detach the child")
	}
}

// Same hazard TestConfigureGroupPreservesExistingFlags guards: the sentinel is a
// flag ConfigureNoWindow does not set itself, so the check fails if CreationFlags
// is assigned rather than ORed.
func TestConfigureNoWindowPreservesExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_DEFAULT_ERROR_MODE}
	ConfigureNoWindow(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_DEFAULT_ERROR_MODE == 0 {
		t.Error("ConfigureNoWindow must preserve a pre-existing CreationFlag")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("ConfigureNoWindow must add CREATE_NO_WINDOW")
	}
}

// TestStartDetachedRunsAndSetsFlags: StartDetached must actually start the process
// and mark it detached (no inherited console).
func TestStartDetachedRunsAndSetsFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	if err := StartDetached(cmd); err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	if cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS == 0 {
		t.Error("StartDetached must set DETACHED_PROCESS")
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

// TestTrackTerminateKillsProcess: a Group captured after Start terminates the
// tracked process. Without the kill the sleeper would run ~60s; cmd.Wait must
// return promptly once the Job Object is terminated.
func TestTrackTerminateKillsProcess(t *testing.T) {
	cmd := startSleeper(t)
	g, err := Track(cmd, true)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if g.job == 0 {
		t.Fatal("Track should have created a Job Object for the tracked process")
	}
	if err := g.Terminate(syscall.SIGKILL); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done: // killed; Wait returned
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Terminate did not kill the tracked process")
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestCloseKillsWhenKillOnClose: with killOnClose=true, closing the Group's last
// Job Object handle terminates the tracked tree. This is the guarantee that agy
// (and its descendants) die if the supervisor process itself dies unexpectedly.
func TestCloseKillsWhenKillOnClose(t *testing.T) {
	cmd := startSleeper(t)
	g, err := Track(cmd, true)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if g.job == 0 {
		t.Skip("no Job Object was created; kill-on-close does not apply")
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("closing the kill-on-close job did not terminate the process")
	}
}

// TestTerminateFallbackWithoutJob: when Track could not create a Job Object, the
// Group keeps only the pid and Terminate degrades to killing that single process.
// Constructing a job-less Group directly exercises that fallback.
func TestTerminateFallbackWithoutJob(t *testing.T) {
	cmd := startSleeper(t)            // startSleeper's cleanup reaps the process
	g := &Group{pid: cmd.Process.Pid} // job == 0: force the single-process fallback
	if err := g.Terminate(syscall.SIGKILL); err != nil {
		t.Fatalf("fallback Terminate: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("fallback Terminate did not kill the process")
	}
}

// TestTerminateRejectsUnusable: a nil group, or a job-less group with a
// non-positive pid, has nothing valid to terminate and must not act.
func TestTerminateRejectsUnusable(t *testing.T) {
	var g *Group
	if err := g.Terminate(syscall.SIGKILL); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("nil Group.Terminate = %v, want EINVAL", err)
	}
	g = &Group{pid: 0}
	if err := g.Terminate(syscall.SIGKILL); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("Group{pid:0}.Terminate = %v, want EINVAL", err)
	}
}

func TestTrackRequiresStart(t *testing.T) {
	if _, err := Track(exec.Command("cmd.exe"), true); err == nil {
		t.Fatal("Track before Start must return an error")
	}
}
