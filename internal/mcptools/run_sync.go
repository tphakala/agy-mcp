package mcptools

import (
	"context"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

const (
	// syncWaitCeiling is the built-in inline-wait ceiling for agy_run_sync and
	// agy_wait. It sits well below the ~120s per-call timeout common MCP clients
	// (Claude Code among them) impose on a single tool call. Staying under that
	// ceiling is what lets agy-mcp's own graceful still-running result reach the
	// caller before the client abandons the call: that result carries the job_id
	// needed to reconcile the run with agy_wait or agy_status, whereas the client's
	// synthesized "timed out" failure discards agy-mcp's response and the job_id
	// with it, orphaning a job that is still running under its detached supervisor.
	//
	// The measured client cap is not perfectly deterministic (in one session a call
	// was abandoned at ~120s while another returned at ~157s), and progress
	// notifications do not extend it, so the ceiling keeps a wide margin rather than
	// sitting at the edge. See issue #178 for the characterization.
	syncWaitCeiling = 90 * time.Second

	// envSyncWaitCap overrides syncWaitCeiling for a client whose per-call timeout
	// differs from Claude Code's: one that tolerates longer calls can raise it, a
	// stricter one can lower it. The value is a Go duration; anything that does not
	// parse as a positive duration is ignored and the built-in ceiling stands.
	envSyncWaitCap = "AGY_MCP_SYNC_WAIT_CAP"
)

// maxSyncWait resolves the effective inline-wait ceiling: AGY_MCP_SYNC_WAIT_CAP
// when it parses as a positive Go duration, otherwise the built-in
// syncWaitCeiling. It is both the clamp for a caller-supplied wait and the default
// when the caller names none (parseWait), so agy_run_sync and agy_wait cannot
// drift on either. Longer runs are for agy_run + agy_wait/agy_status, re-issued
// until the job is terminal.
func maxSyncWait() time.Duration {
	if v := os.Getenv(envSyncWaitCap); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return syncWaitCeiling
}

// runSyncInput is runInput plus the inline wait cap.
type runSyncInput struct {
	runInput
	Wait string `json:"wait,omitempty" jsonschema:"max time to block inline (Go duration, default 90s); a larger value is silently clamped to the ceiling, which stays below common MCP clients' ~120s per-call timeout so the still-running result (carrying the job_id) is delivered inline. Caps only the inline wait, not the job itself: on overrun the job keeps running and the returned job_id can be waited on with agy_wait or polled with agy_status, so never re-send the prompt"`
}

type runSyncOutput struct {
	JobID string `json:"job_id" jsonschema:"handle for this run; still valid after the inline wait runs out, for agy_wait, agy_status or agy_cancel"`
	statusOutput
	Note string `json:"note,omitempty" jsonschema:"set when the job outlived the inline wait, explaining that it is still running and how to collect it"`
}

// registerRunSync adds the agy_run_sync tool: start a job, wait inline for it
// (bounded), streaming progress notifications when the client asked for them.
func registerRunSync(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyRunSync,
		Title:       "Delegate to agy (wait inline)",
		Annotations: annDelegate,
		Description: "Delegate a prompt to an agy model and wait for the result inline (bounded by wait). " +
			"Use when the answer is needed before your next step AND the task is bounded enough to finish within the wait: " +
			"a focused peer review, a second opinion, a rubber-duck question. " +
			"For open-ended work that routinely runs past the wait cap (web research, a whole-codebase review), " +
			"and for parallel work, prefer agy_run. If this call outlives its wait the job keeps running: " +
			"block on the returned job_id with agy_wait, or take a single non-blocking look with agy_status. " +
			"If a transport failure could make job creation ambiguous, supply idempotency_key and reuse it on the retry. " +
			"Streams progress notifications while waiting when the client asks for them. " +
			"The delegated agent runs with permission checks disabled: it can edit files under cwd and under any " +
			"dirs, and may reach the network. Say so in the prompt if the run must not touch the repo.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in runSyncInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait, err := parseWait(in.Wait)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		startReq, err := in.toStartRequest()
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		job, err := mgr.StartJob(startReq)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		out, err := awaitJob(ctx, req, mgr, job.ID, time.Now().Add(wait))
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		return nil, out, nil
	})
}
