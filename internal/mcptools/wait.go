package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// waitInput is the input for agy_wait.
type waitInput struct {
	JobID string `json:"job_id" jsonschema:"the job id to wait for"`
	Wait  string `json:"wait,omitempty" jsonschema:"max time to wait inline (Go duration, default 2m, max 10m); on overrun the job keeps running and can be waited on again or polled with agy_status"`
}

// registerWait adds the agy_wait tool: block on an existing job until it
// finishes (bounded), streaming progress notifications when the client asked
// for them. It shares agy_run_sync's wait phase, so one agy_wait call
// replaces an agy_status poll loop.
func registerWait(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name: toolAgyWait,
		Description: "Block until an agy_run job finishes (bounded by wait, default 2m, max 10m). " +
			"Sends MCP progress notifications while waiting. Prefer one agy_wait over repeated " +
			"agy_status polling when the next step depends on the job's result. If the job " +
			"outlives the wait cap it keeps running; call agy_wait again or poll agy_status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait, err := parseWait(in.Wait)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		// Reject an unknown job before entering the wait loop, so a typo'd id
		// fails fast instead of polling a job that can never appear.
		if _, err := mgr.Status(in.JobID); err != nil {
			return nil, runSyncOutput{}, fmt.Errorf("job %s: %w", in.JobID, err)
		}
		out, err := awaitJob(ctx, req, mgr, in.JobID, time.Now().Add(wait))
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		return nil, out, nil
	})
}
