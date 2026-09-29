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

func TestMarkDismissedRoundTrip(t *testing.T) {
	m := newManager(t, managerOpts{})
	id := "job-dismissed-1"
	createJob(t, m, id)
	if m.Dismissed(id) {
		t.Fatal("Dismissed = true before MarkDismissed")
	}
	if err := m.MarkDismissed(id); err != nil {
		t.Fatalf("MarkDismissed: %v", err)
	}
	if !m.Dismissed(id) {
		t.Fatal("Dismissed = false after MarkDismissed")
	}
	if m.Collected(id) {
		t.Fatal("MarkDismissed also made the job read as collected")
	}
}
