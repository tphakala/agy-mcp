package manager

// MarkCollected records that a client has been handed the job's terminal
// outcome, or asked to cancel it, so a later hook-wait wake would carry nothing
// new (issue #194). Callers must invoke it only after the outcome is in the
// tool response: the marker suppresses a wake, so writing it early loses one.
// Status and WaitTerminal never write it themselves, because hook-wait reads
// through them and would otherwise suppress its own wake.
//
// The write is best effort for the caller: a failure leaves the marker absent,
// which only means a redundant wake.
func (m *Manager) MarkCollected(id string) error {
	return m.store.MarkCollected(id)
}

// Collected reports whether MarkCollected was recorded for the job.
func (m *Manager) Collected(id string) bool {
	return m.store.Collected(id)
}
