package manager

import (
	"context"
	"time"
)

// waitPollInterval is how often WaitTerminal re-reads job status and invokes
// onTick. Status reads a few small files, so this is cheap. agy_run_sync's
// wait loop used the same 250ms cadence before it moved here.
const waitPollInterval = 250 * time.Millisecond

// captureGraceWindow bounds how long after a job completes WaitTerminal keeps
// polling a done, id-less job for a waiter that does not own the in-process
// capture state (a cross-process hook-wait or wait-job, whose pending/settled
// maps are empty). It must comfortably exceed the manager's captureBudget (2s)
// so such a waiter outlasts the owning server's capture retry, and it bounds the
// extra polling a genuinely id-less fresh run can cost that waiter.
const captureGraceWindow = 5 * time.Second

// WaitTerminal polls the job until it reaches a terminal state, the deadline
// passes, or ctx is cancelled. It returns the latest observed Status and
// whether the job was terminal when the wait ended. The deadline is observed
// on poll ticks only, so a wait can overshoot it by up to one poll interval.
//
// onTick, if non-nil, is invoked once per poll from the calling goroutine
// while the job is still running; it must not block longer than the poll
// interval (it is for progress reporting, not work).
//
// One refinement carried over from the original agy_run_sync loop: when the job
// is done but its conversation id is still being captured (see inCaptureGrace),
// WaitTerminal keeps polling until the capture concludes or the deadline passes,
// so a caller that will never poll again does not lose the id to agy's
// cache-flush lag. If ctx is cancelled during that grace the job is already
// terminal, so the status is returned with a nil error. The grace also covers a
// cross-process waiter (which never observes CapturePending) via a
// completion-recency window; for such a waiter Status's lazy capture, which
// reads agy's cache directly, is what actually delivers the id.
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
				if m.inCaptureGrace(id) {
					select {
					case <-ctx.Done():
						return st, true, nil
					case <-ticker.C:
					}
					continue
				}
				// The grace no longer applies. CapturePending may have flipped false
				// between the Status read above and this check: the completion
				// goroutine persists the captured id and only then clears
				// pendingCaptures, so the st we hold can predate a just-settled id.
				// Re-read once so that id is delivered instead of a stale empty one;
				// keep the original st if the re-read fails.
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

// inCaptureGrace reports whether a done, id-less job should keep being polled so
// a late-flushed conversation id is not missed. For the process that owns the
// capture, CapturePending is the precise signal while the eager attempt runs, and
// captureConcluded marks it finished, so an in-process waiter stops as soon as the
// eager attempt concludes. A cross-process waiter has neither (its maps are
// empty), so it falls back to a completion-recency window: while the job finished
// within captureGraceWindow and no local eager attempt concluded it, keep waiting
// for Status's lazy capture (which keeps retrying past the eager attempt) to
// deliver the id.
func (m *Manager) inCaptureGrace(id string) bool {
	if m.CapturePending(id) {
		return true
	}
	if m.captureConcluded(id) {
		return false
	}
	end, ok := m.store.CompletedAt(id)
	return ok && time.Since(end) < captureGraceWindow
}
