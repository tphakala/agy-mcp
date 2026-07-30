package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// waitJobMain implements "agy-mcp wait-job [-timeout 1h] <job_id>": block
// until the job reaches a terminal state, print that state word to stdout,
// and exit 0. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout, 130
// interrupted. It is the scriptable face of manager.WaitTerminal for shell
// automation that would otherwise poll agy_status.
func waitJobMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait-job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: agy-mcp wait-job [-timeout 1h] <job_id>")
		return 2
	}
	id := fs.Arg(0)
	st, terminal, err := waitForJob(id, *timeout)
	if err != nil {
		// signal.NotifyContext cancels the wait's ctx on SIGINT or SIGTERM, which
		// WaitTerminal surfaces as context.Canceled; report that as the POSIX
		// 128+SIGINT interrupt code (130) rather than a generic error, so a script
		// can tell "the user hit Ctrl-C" apart from a real failure. NotifyContext
		// covers SIGTERM too, but one code keeps the contract simple and 130 is the
		// widely understood interrupt exit status.
		if errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintln(stderr, "wait-job: interrupted")
			return 130
		}
		_, _ = fmt.Fprintf(stderr, "wait-job: %v\n", err)
		return 1
	}
	if !terminal {
		_, _ = fmt.Fprintf(stderr, "wait-job: job %s still running after %s\n", id, *timeout)
		return 3
	}
	_, _ = fmt.Fprintln(stdout, st.State)
	return 0
}

// resolveWaitManager builds the wait-only manager (the lighter ResolveWait
// config plus manager.New) shared by the wait subcommands. Both wait-job and
// hook-wait observe the job store without ever execing agy, so they resolve the
// wait config rather than the full one.
func resolveWaitManager() (*manager.Manager, error) {
	cfg, err := config.ResolveWait()
	if err != nil {
		return nil, err
	}
	return manager.New(cfg), nil
}

// waitForJob resolves the wait-only manager and blocks on the job. Shared by
// wait-job and hook-wait so the two subcommands cannot diverge on how a wait
// manager is built. The job itself is never signalled: SIGINT/SIGTERM cancel
// only this observer's wait.
func waitForJob(id string, timeout time.Duration) (manager.Status, bool, error) {
	mgr, err := resolveWaitManager()
	if err != nil {
		return manager.Status{}, false, err
	}
	return waitForJobWith(mgr, id, timeout)
}

// waitForJobWith blocks on the job using an already-resolved manager, so a
// caller that needs the manager for more than the wait (hook-wait reuses it for
// its run_sync short-circuit Status read) resolves it once and reuses it here.
// SIGINT/SIGTERM cancel only this observer's wait, never the job.
func waitForJobWith(mgr *manager.Manager, id string, timeout time.Duration) (manager.Status, bool, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// The handler is installed by the time NotifyContext returns, so from here a
	// SIGINT or SIGTERM cancels this wait instead of killing the process. That is
	// the moment the readiness file announces.
	signalWaitReady()
	return mgr.WaitTerminal(ctx, id, time.Now().Add(timeout), nil)
}

// waitReadyFileEnv names an optional file the wait subcommands create once the
// SIGINT/SIGTERM handler above is installed.
//
// A caller that means to interrupt a wait has no other way to know when that
// handler is in place, and the window before it is not negligible: flag parsing,
// config resolution and manager construction all run first. A signal that lands
// in that window meets the default disposition and kills the process outright,
// losing the exit contract the signal was sent to exercise (130 for wait-job, an
// exit-2 wake for hook-wait). Waiting for this file to appear before signalling
// closes the window; the alternative, sleeping a margin and hoping, is only
// probabilistically safe and was the cause of issue #86.
//
// Unset by default, in which case nothing is written and nothing changes.
const waitReadyFileEnv = "AGY_MCP_WAIT_READY_FILE"

// signalWaitReady creates the file named by AGY_MCP_WAIT_READY_FILE, if the
// variable is set. Errors are deliberately swallowed: the file is a courtesy to
// a parent process and must never derail the wait it announces. A caller that
// never sees the file is left with whatever timeout it would have needed anyway.
func signalWaitReady() {
	path := os.Getenv(waitReadyFileEnv)
	if path == "" {
		return
	}
	_ = os.WriteFile(path, nil, 0o600)
}
