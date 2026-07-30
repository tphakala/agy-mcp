//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// startRunningJobForWait starts a real job under a fake agy that sleeps for the
// given duration and a fake supervisor that records convLabel in the agy
// conversation cache. It points AGY_MCP_STATE_DIR at the job's state dir and
// registers a cleanup that drains the job (within drainTimeout) so the slow fake
// agy never outlives the test. It returns the job id. Shared by the wait-job and
// hook-wait tests that need an observably-running job (the timeout and
// signal-interrupt paths), which is why the fixture writes shell-script fakes and
// stays posix-tagged. It assumes the caller already set a fake home (setFakeHome)
// for the wait manager the subcommand under test resolves.
func startRunningJobForWait(t *testing.T, sleep time.Duration, convLabel string, drainTimeout time.Duration) string {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Exit: 0, Sleep: sleep})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, convLabel),
	})
	stateDir := t.TempDir()
	// ConversationIDWait 0: this fake supervisor writes no progress file, so StartJob
	// would sleep out the whole default budget on every start, which for a short
	// fake agy is long enough for the job to finish before StartJob even returns.
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4, ConversationIDWait: 0,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)

	job, err := mgr.StartJob(manager.StartRequest{Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain the started job once the test is done, so the slow fake agy never
	// outlives the test.
	t.Cleanup(func() {
		testutil.WaitFor(t, drainTimeout, func() bool {
			st, err := mgr.State(job.ID)
			return err == nil && st != manager.StateRunning
		}, "job did not finish before test cleanup")
	})

	t.Setenv("AGY_MCP_STATE_DIR", stateDir)
	return job.ID
}

// armWaitReady wires the readiness handshake into a wait subcommand about to be
// started as a child process, and returns a function that blocks until the child
// has installed its SIGINT/SIGTERM handler. Call it before cmd.Start, then call
// the returned function before signalling.
//
// Signalling any earlier is the issue #86 race: until the handler is installed
// the default disposition kills the child, which reports as exit code -1 with
// empty output rather than the exit code under test. cmd.Env has to be set
// explicitly because the fixtures above use t.Setenv, so the child needs the
// current process environment plus this one variable.
func armWaitReady(t *testing.T, cmd *exec.Cmd) func() {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "wait-ready")
	cmd.Env = append(os.Environ(), waitReadyFileEnv+"="+ready)
	return func() {
		t.Helper()
		testutil.WaitFor(t, 10*time.Second, func() bool {
			_, err := os.Stat(ready)
			return err == nil
		}, "child never signalled that its signal handler is installed")
	}
}
