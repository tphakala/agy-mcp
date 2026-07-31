package manager

import (
	"context"
	"time"
)

// WaitPollInterval is how often WaitTerminal re-reads job status and invokes
// onTick. Status reads a few small files, so this is cheap. It is exported so
// callers that bound per-tick work (mcptools' progress-notification send) share
// the one cadence rather than hard-coding a matching literal.
const WaitPollInterval = 250 * time.Millisecond

// WaitTerminal polls the job until it reaches a terminal state, the deadline
// passes, or ctx is cancelled. It returns the latest observed Status and
// whether the job was terminal when the wait ended. The deadline is observed
// on poll ticks only, so a wait can overshoot it by up to one poll interval.
//
// onTick, if non-nil, is invoked once per poll from the calling goroutine
// while the job is still running; it must not block longer than the poll
// interval (it is for progress reporting, not work).
//
// A terminal status is returned as soon as it is observed. Earlier versions
// kept polling a done, id-less job to outlast agy's conversation-cache flush
// lag, because the id was inferred from that cache after the run. The
// supervisor now records the id from agy's init event long before the job ends,
// so a terminal job's id is already on disk and there is nothing left to wait
// for.
func (m *Manager) WaitTerminal(ctx context.Context, id string, deadline time.Time, onTick func(Status)) (Status, bool, error) {
	ticker := time.NewTicker(WaitPollInterval)
	defer ticker.Stop()
	for {
		st, err := m.Status(id)
		if err != nil {
			return Status{}, false, err
		}
		if st.State != StateRunning {
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
