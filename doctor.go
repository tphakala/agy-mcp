package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// doctorMain runs the `agy-mcp doctor` preflight: it resolves configuration,
// runs the read-only checks a run depends on (agy binary, version, auth,
// state dir, config sources, stale jobs), prints one line per check, and returns
// an exit code a setup script or CI can branch on: 0 when nothing is broken
// (warnings included), 1 when a check failed, and 2 on a usage error (an
// unexpected argument or a bad flag).
//
// It takes its writers rather than using os.Stdout/os.Stderr directly so a test
// can capture the report. It never starts a job or mutates job state; the only
// write is the state-dir writability probe, which cleans up after itself.
func doctorMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: agy-mcp doctor")
		_, _ = fmt.Fprintln(stderr, "Run read-only preflight checks and exit non-zero if something is broken.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "doctor: unexpected argument %q; doctor takes none\n", fs.Arg(0))
		return 2
	}

	// A bad explicit AGY_MCP_AGY_PATH (or an unresolvable state dir) fails here,
	// before a manager exists. That is itself a preflight failure worth a clear
	// message and a non-zero exit, so report it as one rather than panicking.
	cfg, err := config.Resolve()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "doctor: config: %v\n", err)
		return 1
	}

	report := manager.New(cfg).Doctor(context.Background())
	for _, c := range report.Checks {
		_, _ = fmt.Fprintf(stdout, "[%s] %s: %s\n", c.Status, c.Name, c.Detail)
	}
	if report.OK() {
		_, _ = fmt.Fprintln(stdout, "\ndoctor: all checks passed")
		return 0
	}
	_, _ = fmt.Fprintln(stdout, "\ndoctor: one or more checks failed")
	return 1
}
