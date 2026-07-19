package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/internal/hookinput"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/mcptools"
)

// hookWaitMain implements "agy-mcp hook-wait [-timeout 1h]": read a Claude
// Code PostToolUse hook payload from stdin, wait for the agy job it
// references, and exit 2 with a wake message on stderr. Exit 2 is Claude
// Code's asyncRewake wake signal; every other outcome exits 0 with no output,
// because a hook that fails must never disrupt the tool flow it observes.
// A timeout also wakes (exit 2): the model should learn the job is
// long-running rather than never hearing back.
//
// Claude Code renders any exit-2 hook under a "Stop hook blocking error from
// command ..." wrapper it prepends itself; we cannot change that wrapper, only
// the stderr body below. Each wake message therefore leads with an explicit
// "(not an error)" framing so the completion signal is not misread as a
// failure of the observed tool call. Keep that framing on every wake message.
func hookWaitMain(args []string, stdin io.Reader, stderr io.Writer) int {
	// Manager internals log via the std logger on rare error paths; any stray line
	// would land in the transcript on a non-wake exit or prepend noise to the wake
	// message, so silence the std logger for the whole hook run.
	log.SetOutput(io.Discard)
	fs := flag.NewFlagSet("hook-wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage noise would land in the transcript; stay quiet
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	jobID, toolName, respState, ok := hookinput.Parse(stdin)
	if !ok {
		return 0
	}
	// Resolve the wait manager once and reuse it for both the run_sync
	// short-circuit and the wait below. A resolve failure means there is no way to
	// observe the job, so stay quiet rather than wake with nothing to report.
	mgr, err := resolveWaitManager()
	if err != nil {
		return 0
	}
	// A sync tool that already returned a terminal result delivered it inline, so
	// waking Claude again would be noise. Suppress only when all three hold: the
	// tool is agy_run_sync, its recorded response state is a KNOWN terminal state
	// (not "" and not running), AND the job is terminal on disk now. An absent or
	// unknown payload state must fail toward waking: a "running" response means the
	// sync call overran its wait cap and delivered no result inline (the wake is
	// owed no matter what the live status now says), and an empty/odd state is not
	// proof of inline delivery either. A redundant wake is only noise; a suppressed
	// owed wake is a lost result. agy_run never delivers inline, so it always wakes.
	if strings.HasSuffix(toolName, mcptools.ToolAgyRunSync) &&
		respState != "" && respState != manager.StateRunning {
		if st, err := mgr.Status(jobID); err == nil && st.State != manager.StateRunning {
			return 0
		}
	}
	st, terminal, err := waitForJobWith(mgr, jobID, *timeout)
	if err != nil {
		// An externally delivered SIGINT/SIGTERM cancels only this observer, not the
		// job, which keeps running under its detached supervisor. Exiting 0 would
		// silently drop an owed wake, contradicting the documented guarantee, so wake
		// with a distinct message and point the model at agy_wait. Only cancellation
		// wakes here; a genuine internal error still exits 0 to stay out of the tool
		// flow. When the outer hook timeout killed this process the parent has already
		// stopped listening, so the extra exit 2 is harmless.
		if !errors.Is(err, context.Canceled) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "agy async job notification (not an error): job %s wait interrupted; the job may still be running; wait for it with agy_wait or check agy_status with this job_id\n", jobID)
		return 2
	}
	if terminal {
		_, _ = fmt.Fprintf(stderr, "agy async job notification (not an error): job %s finished: state=%s elapsed=%s; call agy_status with this job_id to collect the result\n",
			jobID, st.State, st.Elapsed.Round(time.Second))
	} else {
		// Lead with agy_status here, unlike the tool-level note: this branch only
		// fires once the job has already outrun hook-wait's own timeout (1h by
		// default), so agy_wait's 10m cap would most likely just overrun again. One
		// cheap status read is the better first move for a job this long-lived.
		_, _ = fmt.Fprintf(stderr, "agy async job notification (not an error): job %s still running after %s; check agy_status, or call agy_wait to block for another bounded window\n", jobID, *timeout)
	}
	return 2
}
