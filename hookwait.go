package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/hookinput"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// hookWaitMain implements "agy-mcp hook-wait [-timeout 1h]": read a Claude
// Code PostToolUse hook payload from stdin, wait for the agy job it
// references, and exit 2 with a wake message on stderr. Exit 2 is Claude
// Code's asyncRewake wake signal; every other outcome exits 0 with no output,
// because a hook that fails must never disrupt the tool flow it observes.
// A timeout also wakes (exit 2): the model should learn the job is
// long-running rather than never hearing back.
func hookWaitMain(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook-wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage noise would land in the transcript; stay quiet
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	jobID, toolName, ok := hookinput.Parse(stdin)
	if !ok {
		return 0
	}
	// A sync tool that already returned a terminal result delivered it inline;
	// waking Claude again would be noise. agy_run never delivers inline, so
	// for it even an already-terminal job still gets its wake.
	if strings.HasSuffix(toolName, "agy_run_sync") {
		if cfg, err := config.ResolveWait(); err == nil {
			if st, err := manager.New(cfg).Status(jobID); err == nil && st.State != manager.StateRunning {
				return 0
			}
		}
	}
	st, terminal, err := waitForJob(jobID, *timeout)
	if err != nil {
		return 0
	}
	if terminal {
		_, _ = fmt.Fprintf(stderr, "agy job %s finished: state=%s elapsed=%s; call agy_status with this job_id to collect the result\n",
			jobID, st.State, st.Elapsed.Round(time.Second))
	} else {
		_, _ = fmt.Fprintf(stderr, "agy job %s still running after %s; poll agy_status\n", jobID, *timeout)
	}
	return 2
}
