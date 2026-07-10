package testutil

import (
	"testing"
	"time"
)

// pollInterval is the fixed cadence WaitFor polls cond at. 10ms matches the
// granularity most call sites already used before consolidation; it is fine
// enough not to add meaningful latency and coarse enough not to spin the CPU.
const pollInterval = 10 * time.Millisecond

// WaitFor polls cond every pollInterval until it returns true, then returns.
// If timeout elapses first, it fails the test with msg. cond may itself call
// t.Fatal for a hard, non-retryable error instead of returning false.
func WaitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(pollInterval)
	}
}
