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
	// session indefinitely; longer runs are for agy_run + agy_status.
	maxSyncWait = 10 * time.Minute
)

// runSyncInput is runInput plus the inline wait cap.
type runSyncInput struct {
	runInput
	Wait string `json:"wait,omitempty" jsonschema:"max time to wait inline (Go duration, default 2m, max 10m); on overrun the job keeps running and the job_id is returned for agy_status polling"`
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
		Name: toolAgyRunSync,
		Description: "Start an agy prompt and wait for it inline (bounded by wait, default 2m). " +
			"Sends MCP progress notifications while waiting. If the job outlives the wait cap " +
			"it keeps running and the returned job_id can be polled with agy_status.",
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
