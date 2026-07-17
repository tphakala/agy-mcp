package mcptools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// notifyTimeout bounds each progress-notification send so a client that
// stopped draining the stream cannot park the wait loop past its cap. It
// matches the manager's poll cadence, which is what the send used to be
// bounded by when the loop lived in run_sync.
const notifyTimeout = 250 * time.Millisecond

// parseWait validates a caller-supplied inline wait, applying the shared
// default and cap. agy_run_sync and agy_wait use it so the two cannot drift.
func parseWait(s string) (time.Duration, error) {
	if s == "" {
		return defaultSyncWait, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid wait %q: want a positive Go duration like 90s", s)
	}
	return min(d, maxSyncWait), nil
}

// awaitJob blocks until the job is terminal or the deadline passes, emitting
// once-per-second MCP progress notifications when the client supplied a
// progress token. It is the wait phase shared by agy_run_sync and agy_wait,
// so their semantics cannot drift. On a wait-cap overrun the returned output
// carries the standard poll-with-agy_status note.
func awaitJob(ctx context.Context, req *mcp.CallToolRequest, mgr *manager.Manager, jobID string, deadline time.Time) (runSyncOutput, error) {
	token := req.Params.GetProgressToken()
	// Notify once per elapsed second, not per poll tick: the message has
	// whole-second granularity, so finer cadence is pure stream noise.
	lastNotified := time.Duration(-1)
	onTick := func(st manager.Status) {
		sec := st.Elapsed.Truncate(time.Second)
		if token == nil || sec == lastNotified {
			return
		}
		lastNotified = sec
		// Best effort: the result, not the notifications, is the contract.
		nctx, ncancel := context.WithTimeout(ctx, notifyTimeout)
		_ = req.Session.NotifyProgress(nctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      st.Elapsed.Seconds(),
			Message:       fmt.Sprintf("job %s running (%s)", jobID, sec),
		})
		ncancel()
	}
	st, terminal, err := mgr.WaitTerminal(ctx, jobID, deadline, onTick)
	if err != nil {
		// Classify by error identity, not context state: ctx can already be
		// Done() in the same poll tick as an unrelated Status() read failure,
		// so checking ctx.Err() != nil alone would mislabel that failure as a
		// cancellation. Only report cancellation when WaitTerminal's error is
		// actually the context error.
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			// The client gave up on the call; the job stays alive under its
			// detached supervisor. Carry the job id in the error so a
			// gracefully-cancelling client can still find the job.
			return runSyncOutput{}, fmt.Errorf("wait cancelled; job %s is still running, poll it with agy_status: %w", jobID, err)
		}
		return runSyncOutput{}, fmt.Errorf("job %s status read failed: %w", jobID, err)
	}
	out := runSyncOutput{JobID: jobID, statusOutput: toStatusOutput(st)}
	if !terminal {
		out.Note = "wait cap reached; the job is still running, poll it with agy_status"
	}
	return out, nil
}
