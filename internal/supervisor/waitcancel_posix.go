//go:build linux || darwin

package supervisor

import (
	"os"
	"os/signal"
	"syscall"
)

// waitForCancel reports a manager cancel to Run. On Linux and macOS the manager
// sends the supervisor SIGTERM, so this installs a SIGTERM handler and closes
// the returned channel when one arrives. The signal channel is buffered so a
// SIGTERM that lands before the forwarding goroutine reads it is held, not
// lost; Run installs the handler before creating the job files, so the
// "out/err exist" readiness barrier tests rely on also means the cancel handler
// is already in place.
//
// stop releases the handler and the goroutine; Run defers it. The jobDir is
// unused on Linux and macOS (the signal carries the request); the Windows
// build polls a sentinel in it.
func waitForCancel(_ string) (cancel <-chan struct{}, stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	ch := make(chan struct{})
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			close(ch)
		case <-done:
		}
	}()
	return ch, func() {
		signal.Stop(sig)
		close(done)
	}
}
