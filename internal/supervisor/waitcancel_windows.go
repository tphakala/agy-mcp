package supervisor

import (
	"os"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// cancelPollInterval is how often the supervisor checks for the cancel sentinel.
// A few hundred ms bounds cancel latency without a busy loop; the hard timeout is
// enforced by the same goroutine's timer, so cancel need not be instant.
const cancelPollInterval = 200 * time.Millisecond

// waitForCancel reports a manager cancel to Run. Windows has no POSIX signal to
// deliver to an arbitrary process, so the manager writes a cancel sentinel file
// into the job dir and this polls for it, closing the returned channel once it
// appears. Run starts this before creating the job files, so the sentinel is
// caught even if it was written before polling began (the file persists, unlike a
// signal), and the "out/err exist" readiness barrier still implies cancel is armed.
//
// stop ends the polling goroutine; Run defers it.
func waitForCancel(jobDir string) (cancel <-chan struct{}, stop func()) {
	ch := make(chan struct{})
	done := make(chan struct{})
	cancelPath := jobstore.CancelPath(jobDir)
	go func() {
		t := time.NewTicker(cancelPollInterval)
		defer t.Stop()
		for {
			if _, err := os.Stat(cancelPath); err == nil {
				close(ch)
				return
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return ch, func() { close(done) }
}
