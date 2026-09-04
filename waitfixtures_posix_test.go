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
	cacheJSON := fmt.Sprintf(`{%q:%q}`, cwd, convLabel)
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: cacheJSON,
	})
	stateDir := t.TempDir()
	// No ConversationIDWait: this fixture starts jobs through StartJob, which does
	// not wait for a conversation id, so the budget never applies here.
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)

	job, err := mgr.StartJob(manager.StartRequest{Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain the started job once the test is done, so the slow fake agy never
	// outlives the test. The fake supervisor writes exit_code before its final
	// cache copy, so State becoming terminal is not enough: TempDir cleanup can
	// otherwise race that trailing write under the race detector. Waiting for the
	// expected cache payload proves the fake has completed its last file write.
	t.Cleanup(func() {
		testutil.WaitFor(t, drainTimeout, func() bool {
			st, err := mgr.State(job.ID)
			return err == nil && st != manager.StateRunning
		}, "job did not finish before test cleanup")
		testutil.WaitFor(t, drainTimeout, func() bool {
			b, err := os.ReadFile(cachePath)
			return err == nil && string(b) == cacheJSON
		}, "fake supervisor did not finish its trailing cache write before test cleanup")
	})

	t.Setenv("AGY_MCP_STATE_DIR", stateDir)
	return job.ID
}

// armWaitReady wires the readiness handshake into a wait subcommand about to be
// started as a child process, and returns a function that blocks until the child
// has installed its SIGINT/SIGTERM handler.
//
// Signalling any earlier is the issue #86 race: until the handler is installed
// the default disposition kills the child, which reports as exit code -1 with
// empty output rather than the exit code under test.
//
// Two ordering requirements, one of them enforced below. It must run before
// cmd.Start, because exec.Cmd reads Env once at Start and a later assignment is
// ignored. It must also run after every t.Setenv the child needs, because
// setting Env at all replaces the inherited environment, and the os.Environ()
// snapshot is taken here rather than at Start. Both fixtures above do their
// t.Setenv work before returning, so callers get this right by writing the
// natural order.
func armWaitReady(t *testing.T, cmd *exec.Cmd) func() {
	t.Helper()
	if cmd.Process != nil {
		t.Fatal("armWaitReady must be called before cmd.Start: exec.Cmd reads Env at Start")
	}
	ready := filepath.Join(t.TempDir(), "wait-ready")
	// Append rather than overwrite. No current caller sets cmd.Env first, but tests
	// in this package do build it by hand (serve_noagy_test.go), so a future caller
	// combining the two would otherwise lose its variables with nothing to see.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, waitReadyFileEnv+"="+ready)
	return func() {
		t.Helper()
		testutil.WaitFor(t, 10*time.Second, func() bool {
			_, err := os.Stat(ready)
			return err == nil
		}, "child never signalled that its signal handler is installed; it may have exited early, "+
			"so check the exit code and stderr reported by the assertions below")
	}
}
