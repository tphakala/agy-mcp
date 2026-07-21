package mcptools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

const (
	// defaultSyncWait bounds how long agy_run_sync blocks when the caller
	// does not say otherwise; quick models finish well inside it.
	defaultSyncWait = 2 * time.Minute
	// maxSyncWait caps caller-supplied waits so a tool call cannot park a
	// session indefinitely; longer runs are for agy_run + agy_wait/agy_status.
	maxSyncWait = 10 * time.Minute
)

// runSyncInput is runInput plus the inline wait cap.
type runSyncInput struct {
	runInput
	Wait string `json:"wait,omitempty" jsonschema:"max time to block inline (Go duration, default 2m); a larger value is silently clamped to 10m. Caps only the inline wait, not the job itself: on overrun the job keeps running and the returned job_id can be waited on with agy_wait or polled with agy_status, so never re-send the prompt"`
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
