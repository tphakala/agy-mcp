package mcptools

import (
	"context"
	"fmt"
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
	// syncPollInterval is how often the wait loop re-reads job status and
	// emits a progress notification. Status reads a few small files, so
	// this is cheap.
	syncPollInterval = 250 * time.Millisecond
)

// runSyncInput is runInput plus the inline wait cap.
type runSyncInput struct {
	runInput
	Wait string `json:"wait,omitempty" jsonschema:"max time to wait inline (Go duration, default 2m, max 10m); on overrun the job keeps running and the job_id is returned for agy_status polling"`
}

type runSyncOutput struct {
	JobID          string `json:"job_id"`
	State          string `json:"state"`
	Elapsed        string `json:"elapsed"`
	Result         string `json:"result,omitempty"`
	Error          string `json:"error,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Note           string `json:"note,omitempty"`
}

// registerRunSync adds the agy_run_sync tool: start a job, wait inline for it
// (bounded), streaming progress notifications when the client asked for them.
func registerRunSync(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "agy_run_sync",
		Description: "Start an agy prompt and wait for it inline (bounded by wait, default 2m). " +
			"Sends MCP progress notifications while waiting. If the job outlives the wait cap " +
			"it keeps running and the returned job_id can be polled with agy_status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in runSyncInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait := defaultSyncWait
		if in.Wait != "" {
			d, err := time.ParseDuration(in.Wait)
			if err != nil || d <= 0 {
				return nil, runSyncOutput{}, fmt.Errorf("invalid wait %q: want a positive Go duration like 90s", in.Wait)
			}
			wait = min(d, maxSyncWait)
		}
		startReq, err := in.toStartRequest()
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		job, err := mgr.StartJob(startReq)
		if err != nil {
			return nil, runSyncOutput{}, err
		}

		token := req.Params.GetProgressToken()
		deadline := time.Now().Add(wait)
		ticker := time.NewTicker(syncPollInterval)
		defer ticker.Stop()
		for {
			st, err := mgr.Status(job.ID)
			if err != nil {
				return nil, runSyncOutput{}, fmt.Errorf("job %s started but status read failed: %w", job.ID, err)
			}
			out := runSyncOutput{
				JobID: job.ID, State: st.State,
				Elapsed:        st.Elapsed.Round(time.Second).String(),
				Result:         st.Result,
				Error:          st.Error,
				ConversationID: st.ConversationID,
			}
			if st.State != manager.StateRunning {
				return nil, out, nil
			}
			// The cap is approximate: the loop only observes the deadline on a
			// poll tick, so a call can overshoot wait by up to syncPollInterval.
			if time.Now().After(deadline) {
				out.Note = "wait cap reached; the job is still running, poll it with agy_status"
				return nil, out, nil
			}
			if token != nil {
				// Best effort: the result, not the notifications, is the contract.
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token,
					Progress:      st.Elapsed.Seconds(),
					Message:       fmt.Sprintf("job %s running (%s)", job.ID, st.Elapsed.Round(time.Second)),
				})
			}
			select {
			case <-ctx.Done():
				// The client gave up on the call; the job stays alive under
				// its detached supervisor for agy_status polling. Carry the
				// job id in the error so a gracefully-cancelling client can
				// still find the job.
				return nil, runSyncOutput{}, fmt.Errorf("wait cancelled; job %s is still running, poll it with agy_status: %w", job.ID, ctx.Err())
			case <-ticker.C:
			}
		}
	})
}
