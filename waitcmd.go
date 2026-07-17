package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// waitJobMain implements "agy-mcp wait-job [-timeout 1h] <job_id>": block
// until the job reaches a terminal state, print that state word to stdout,
// and exit 0. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout. It is the
// scriptable face of manager.WaitTerminal for shell automation that would
// otherwise poll agy_status.
func waitJobMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait-job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agy-mcp wait-job [-timeout 1h] <job_id>")
		return 2
	}
	id := fs.Arg(0)
	st, terminal, err := waitForJob(id, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "wait-job: %v\n", err)
		return 1
	}
	if !terminal {
		fmt.Fprintf(stderr, "wait-job: job %s still running after %s\n", id, *timeout)
		return 3
	}
	fmt.Fprintln(stdout, st.State)
	return 0
}

// waitForJob resolves the wait-only config and blocks on the job. Shared by
// wait-job and hook-wait so the two subcommands cannot diverge on how a wait
// manager is built. The job itself is never signalled: SIGINT/SIGTERM cancel
// only this observer's wait.
func waitForJob(id string, timeout time.Duration) (manager.Status, bool, error) {
	cfg, err := config.ResolveWait()
	if err != nil {
		return manager.Status{}, false, err
	}
	mgr := manager.New(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return mgr.WaitTerminal(ctx, id, time.Now().Add(timeout), nil)
}
