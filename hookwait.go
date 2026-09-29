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

	"github.com/tphakala/agy-mcp/v2/internal/hookinput"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/mcptools"
)

// hookWaitMain implements "agy-mcp hook-wait [-timeout 1h]": read a Claude
// Code PostToolUse hook payload from stdin, wait for the agy job it
// references, and exit 2 with a wake message on stderr. Exit 2 is Claude
// Code's asyncRewake wake signal; every other outcome exits 0 with no output,
// because a hook that fails must never disrupt the tool flow it observes.
// A timeout also wakes (exit 2): the model should learn the job is
// long-running rather than never hearing back.
//
// A job that finishes is not woken for when its outcome was already handed to the
// session (agy_wait or agy_status recorded the collected marker, issue #194);
// that case exits 0. A finished job with no marker still wakes.
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
	in, ok := hookinput.ParseInput(stdin)
	if !ok {
		return 0
	}
	jobID, toolName, respState := in.JobID, in.ToolName, in.State
	// A job started inside a subagent wakes the parent session too, which cannot
	// otherwise tell the job is not its own. Say so in the finish and still-running
	// messages.
	owner := ""
	if in.AgentType != "" {
		owner = fmt.Sprintf(" (started by subagent %q, so it may not be this session's job)", in.AgentType)
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
		// The session may have collected the outcome itself (agy_wait, agy_status),
		// in which case this wake carries nothing new. Those tools record a marker
		// after their response is built, which can land just
		// after this observer sees the terminal state, so allow a short grace. Only
		// a marker suppresses the wake; anything uncertain still wakes.
		if waitCollected(mgr, jobID, collectedGrace) {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "agy async job notification (not an error): job %s finished%s: state=%s elapsed=%s; call agy_status with this job_id to collect the result\n",
			jobID, owner, st.State, st.Elapsed.Round(time.Second))
	} else {
		// Lead with agy_status here, unlike the tool-level note: this branch only
		// fires once the job has already outrun hook-wait's own timeout (1h by
		// default), so agy_wait's far shorter cap would most likely just overrun
		// again. One cheap status read is the better first move for a job this
		// long-lived.
		_, _ = fmt.Fprintf(stderr, "agy async job notification (not an error): job %s still running after %s%s; check agy_status, or call agy_wait to block for another bounded window\n", jobID, *timeout, owner)
	}
	return 2
}

// collectedGrace is how long hook-wait keeps looking for the collected marker
// after it sees a terminal job. It is a variable so tests can shorten it.
var collectedGrace = time.Second

// collectedPollInterval is how often waitCollected re-checks for the marker.
const collectedPollInterval = 25 * time.Millisecond

// waitCollected reports whether the job's collected marker appears within
// grace. It checks once immediately, so an already-collected job returns without
// sleeping.
func waitCollected(mgr *manager.Manager, jobID string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if mgr.Collected(jobID) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(collectedPollInterval)
	}
}
