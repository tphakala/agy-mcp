package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	return mgr.WaitTerminal(ctx, id, time.Now().Add(timeout), nil)
}
