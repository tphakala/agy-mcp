package supervisor

import (
	"os"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestWaitForCancelFiresOnSentinel(t *testing.T) {
	dir := t.TempDir()
	cancel, stop := waitForCancel(dir)
	defer stop()

	select {
	case <-cancel:
		t.Fatal("cancel fired before the sentinel was written")
	case <-time.After(100 * time.Millisecond):
	}

	if err := os.WriteFile(jobstore.CancelPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancel:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not fire after the sentinel was written")
	}
}

// TestWaitForCancelStopWithoutSentinel: stop must end the polling goroutine
// cleanly when no cancel ever arrives.
func TestWaitForCancelStopWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	cancel, stop := waitForCancel(dir)
	stop()
	// The channel must not have been closed by stop (no cancel was requested).
	select {
	case <-cancel:
		t.Fatal("cancel must not fire when only stop was called")
	case <-time.After(50 * time.Millisecond):
	}
}
