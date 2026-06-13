package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// countingStore records how many times Load and ExitCode are called per job id,
// so a test can prove the fused startup pass reads each job dir once rather than
// the two reads the back-to-back GarbageCollect+RestoreGate scans would do.
type countingStore struct {
	jobStore
	loads     map[string]int
	exitReads map[string]int
}

func newCountingStore(inner jobStore) *countingStore {
	return &countingStore{jobStore: inner, loads: map[string]int{}, exitReads: map[string]int{}}
}

func (c *countingStore) Load(id string) (jobstore.Meta, error) {
	c.loads[id]++
	return c.jobStore.Load(id)
}

func (c *countingStore) ExitCode(id string) (int, bool) {
	c.exitReads[id]++
	return c.jobStore.ExitCode(id)
}

// RestoreAndCollect must read each job's meta.json exactly once, the IO win it
// exists for: the two scans it replaces loaded every job's meta twice (once per
// scan). The single loadJob result is shared by the GC and restore decisions. Each
// decision still reads the exit_code sentinel itself; for these two job categories
// (an expired job GC removes, and a recent job restore inspects) that is one read
// each, which this also asserts. Terminal jobs keep the test pure (no processAlive
// / proc dependency), so it runs on every platform.
func TestRestoreAndCollectScansEachJobOnce(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// An expired terminal job (GC removes it) and a recent terminal job (kept).
	if _, err := m.store.Create(jobstore.Meta{ID: "old-done", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("old-done", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Create(jobstore.Meta{ID: "recent-done", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("recent-done", 0); err != nil {
		t.Fatal(err)
	}
	// Wrap only after creation so the Create-time writes are not counted.
	cs := newCountingStore(m.store)
	m.store = cs

	removed, err := m.RestoreAndCollect()
	if err != nil {
		t.Fatalf("RestoreAndCollect: %v", err)
	}
	if len(removed) != 1 || removed[0] != "old-done" {
		t.Fatalf("removed = %v, want [old-done]", removed)
	}
	for _, id := range []string{"old-done", "recent-done"} {
		if cs.loads[id] != 1 {
			t.Errorf("job %s: meta loaded %d times, want exactly 1 (the fused scan must not re-read)", id, cs.loads[id])
		}
		if cs.exitReads[id] > 1 {
			t.Errorf("job %s: exit_code read %d times, want at most 1", id, cs.exitReads[id])
		}
	}
}

// With JobTTL<=0, collection is disabled, so RestoreAndCollect must remove nothing
// even for a long-expired job, while still completing its (restore) scan. A
// terminal job needs no live supervisor, keeping this pure.
func TestRestoreAndCollectSkipsRemovalWhenTTLDisabled(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4}) // JobTTL 0
	if _, err := m.store.Create(jobstore.Meta{ID: "expired-job", StartedAt: time.Now().Add(-100 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("expired-job", 0); err != nil {
		t.Fatal(err)
	}
	removed, err := m.RestoreAndCollect()
	if err != nil {
		t.Fatalf("RestoreAndCollect: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("TTL 0 must disable removal, removed %v", removed)
	}
	if ids, _ := m.store.List(); len(ids) != 1 || ids[0] != "expired-job" {
		t.Fatalf("the job must survive a TTL-disabled sweep, List = %v", ids)
	}
}

// RestoreAndCollect must fail closed: if the on-disk jobs cannot be scanned the
// gate cannot be made safe, so it returns an error and startup refuses rather than
// serving with an unrestored gate (a stricter contract than GarbageCollect's, whose
// scan failure was only logged).
func TestRestoreAndCollectFailsClosedOnScanError(t *testing.T) {
	state := t.TempDir()
	// Make the jobs path a file so the directory scan errors (distinct from the
	// benign "directory does not exist yet" case, which is not an error).
	if err := os.WriteFile(filepath.Join(state, "jobs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(config.Config{StateDir: state, MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.RestoreAndCollect(); err == nil {
		t.Fatal("RestoreAndCollect must return an error when the jobs dir cannot be scanned")
	}
}
