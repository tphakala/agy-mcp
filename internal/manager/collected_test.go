package manager

import "testing"

// MarkCollected and Collected are the manager's seam over the job-dir marker
// that hook-wait reads (issue #194). Removing the store call from either must
// turn this red.
func TestMarkCollectedRoundTrip(t *testing.T) {
	m := newManager(t, managerOpts{})
	id := "job-collected-1"
	createJob(t, m, id)
	if m.Collected(id) {
		t.Fatal("Collected = true before MarkCollected")
	}
	if err := m.MarkCollected(id); err != nil {
		t.Fatalf("MarkCollected: %v", err)
	}
	if !m.Collected(id) {
		t.Fatal("Collected = false after MarkCollected")
	}
}
