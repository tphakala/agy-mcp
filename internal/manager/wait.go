package manager

import (
	"context"
	"time"
)

// waitPollInterval is how often WaitTerminal re-reads job status and invokes
// onTick. Status reads a few small files, so this is cheap. agy_run_sync's
// wait loop used the same 250ms cadence before it moved here.
const waitPollInterval = 250 * time.Millisecond

// WaitTerminal polls the job until it reaches a terminal state, the deadline
// passes, or ctx is cancelled. It returns the latest observed Status and
// whether the job was terminal when the wait ended. The deadline is observed
// on poll ticks only, so a wait can overshoot it by up to one poll interval.
//
// onTick, if non-nil, is invoked once per poll from the calling goroutine
// while the job is still running; it must not block longer than the poll
// interval (it is for progress reporting, not work).
//
// One refinement carried over from the original agy_run_sync loop: when the
// job is done but its conversation id is still being captured in this process
// (CapturePending), WaitTerminal keeps polling until the capture settles or
// the deadline passes, so a caller that will never poll again does not lose
// the id to agy's cache-flush lag. If ctx is cancelled during that grace the
// job is already terminal, so the status is returned with a nil error.
// Cross-process callers always observe CapturePending == false; for them
// Status lazy-captures on read instead and no grace applies.
func (m *Manager) WaitTerminal(ctx context.Context, id string, deadline time.Time, onTick func(Status)) (Status, bool, error) {
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		st, err := m.Status(id)
		if err != nil {
			return Status{}, false, err
		}
		if st.State != StateRunning {
			if st.State == StateDone && st.ConversationID == "" && time.Now().Before(deadline) {
				if m.CapturePending(id) {
					select {
					case <-ctx.Done():
						return st, true, nil
					case <-ticker.C:
					}
					continue
				}
				// CapturePending went false between the Status read above and this
				// check: the completion goroutine persists the captured id and only
				// then clears pendingCaptures, so the st we hold can predate a
				// just-settled id. Re-read once so that id is delivered instead of a
				// stale empty one; keep the original st if the re-read fails.
				if fresh, ferr := m.Status(id); ferr == nil {
					return fresh, true, nil
				}
			}
			return st, true, nil
		}
		if time.Now().After(deadline) {
			return st, false, nil
		}
		if onTick != nil {
			onTick(st)
		}
		select {
		case <-ctx.Done():
			return st, false, ctx.Err()
		case <-ticker.C:
		}
	}
}
