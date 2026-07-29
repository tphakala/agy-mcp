package mcptools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

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
// carries the standard still-running note, which names agy_wait before
// agy_status.
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
		// Best effort: the result, not the notifications, is the contract. Bound
		// each send to one poll tick so a client that stopped draining the stream
		// cannot park the wait loop past its cap.
		nctx, ncancel := context.WithTimeout(ctx, manager.WaitPollInterval)
		// Name the step agy is on when the stream has reported one, so the message
		// says what the run is doing rather than only how long it has been at it.
		msg := fmt.Sprintf("job %s running (%s)", jobID, sec)
		if st.StepType != "" {
			msg += ": " + st.StepType
		}
		_ = req.Session.NotifyProgress(nctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      st.Elapsed.Seconds(),
			Message:       msg,
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
			return runSyncOutput{}, fmt.Errorf("wait cancelled; job %s is still running, wait for it with agy_wait or check it with agy_status: %w", jobID, err)
		}
		return runSyncOutput{}, fmt.Errorf("job %s status read failed: %w", jobID, err)
	}
	out := runSyncOutput{JobID: jobID, statusOutput: toStatusOutput(st)}
	if !terminal {
		// Name agy_wait first: one blocking call is cheaper than an agy_status poll
		// loop, and the note is the instruction a caller actually acts on at overrun
		// (the tool description is far away by then). Saying the job is still running
		// also has to be unambiguous, or a caller reads "not done" as "failed" and
		// re-sends the same prompt, paying for the work twice.
		out.Note = "wait cap reached; this is not a failure, the job is still running. " +
			"Call agy_wait with this job_id to block until it finishes, or agy_status to check it now. " +
			"Do not re-send the prompt."
	}
	return out, nil
}
