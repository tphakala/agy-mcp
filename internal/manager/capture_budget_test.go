package manager

import "testing"

// captureGraceWindow (wait.go) must stay strictly larger than defaultCaptureBudget
// (the eager in-process capture window). A cross-process waiter has no
// CapturePending signal and relies on the completion-recency window to outlast the
// owning server's capture retry; were the grace window to shrink to or below the
// budget, that waiter could stop polling before the late cache flush lands and
// silently return a done job with an empty conversation id. Untagged so the
// relationship is checked on every platform.
func TestCaptureGraceWindowExceedsBudget(t *testing.T) {
	if captureGraceWindow <= defaultCaptureBudget {
		t.Fatalf("captureGraceWindow (%s) must exceed defaultCaptureBudget (%s); a cross-process waiter would stop polling before the late capture lands",
			captureGraceWindow, defaultCaptureBudget)
	}
}
