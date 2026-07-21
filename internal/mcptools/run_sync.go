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
	Wait string `json:"wait,omitempty" jsonschema:"max time to block inline (Go duration, default 2m, max 10m). Caps only the inline wait, not the job itself: on overrun the job keeps running and the returned job_id can be waited on with agy_wait or polled with agy_status"`
}

type runSyncOutput struct {
	JobID string `json:"job_id"`
	statusOutput
	Note string `json:"note,omitempty"`
}

// registerRunSync adds the agy_run_sync tool: start a job, wait inline for it
// (bounded), streaming progress notifications when the client asked for them.
func registerRunSync(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyRunSync,
		Title:       "Delegate to agy (wait inline)",
		Annotations: annDelegate,
		Description: "Delegate a prompt to an agy model and wait for the result inline (bounded by wait, default 2m, max 10m). " +
			"Use when the answer is needed before your next step AND the task is bounded enough to finish within the wait: " +
			"a focused peer review, a second opinion, a rubber-duck question. " +
			"Sends MCP progress notifications while waiting. Outliving the wait cap is not a failure: the job keeps " +
			"running and the returned job_id resolves through agy_wait or agy_status, so never re-send the prompt. " +
			"For open-ended work that routinely runs past the wait cap (web research, a whole-codebase review), " +
			"and for parallel work, prefer agy_run. " +
			"The delegated agent has web access and can edit files under cwd, so say so in the prompt if the run " +
			"must not touch the repo. Two fresh runs in the same cwd conflict rather than queueing.",
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
