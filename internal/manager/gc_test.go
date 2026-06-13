package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// reReadExitStore reports "no sentinel" on the first ExitCode(id) call and
// "exited 0" on every later call for one job id, simulating a supervisor that
// writes its sentinel and exits between GarbageCollect's two ExitCode checks.
type reReadExitStore struct {
	jobStore
	id    string
	calls int
}

func (r *reReadExitStore) ExitCode(id string) (int, bool) {
	if id == r.id {
		r.calls++
		if r.calls == 1 {
			return 0, false
		}
		return 0, true
	}
	return r.jobStore.ExitCode(id)
}

func TestGarbageCollectReapsExpiredOrphanDir(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// A job dir with no meta.json: a crash between Create's MkdirAll and its meta
	// write, or a partial RemoveAll. Load fails on it, so the old GC skipped it
	// forever and orphan dirs accumulated without bound.
	dir, err := m.store.Dir("orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "orphan" {
		t.Fatalf("removed = %v, want [orphan]", removed)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expired orphan dir should be removed; stat err = %v", err)
	}
}

func TestGarbageCollectKeepsRecentOrphanDir(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// A meta-less dir younger than the TTL may be a job mid-Create (MkdirAll done,
	// meta write pending), so it must not be reaped.
	dir, err := m.store.Dir("fresh-orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("a recent orphan should be kept, removed %v", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("recent orphan dir should remain: %v", err)
	}
}

func TestGarbageCollectKeepsExpiredJobWithUnreadableMeta(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// meta.json is present but unparseable (a transient read error, or a legacy
	// corrupt write). This is NOT an orphan: only a genuinely missing meta.json is.
	// A valid long-running job's dir mtime is old because writing to out/err does
	// not bump the directory mtime, so reaping on mtime here would delete a live job
	// the moment a transient Load error coincided with it being past the TTL.
	dir, err := m.store.Dir("corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("a job with a corrupt-but-present meta must not be reaped, removed %v", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir should remain: %v", err)
	}
}

func TestGarbageCollectKeepsTerminalJobWithCapturePending(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// A fresh run that exited 0 and is past the TTL, but whose conversation-id
	// capture is still in flight: the manager's post-cmd.Wait goroutine is inside
	// captureFreshConversationID, still writing this dir (the sentinel is already on
	// disk because the supervisor writes it before exiting, so the first ExitCode
	// read sees it). Removing the dir now would make SetConversationID fail and lose
	// the captured id, so GC must keep the job until the capture settles.
	if _, err := m.store.Create(jobstore.Meta{ID: "capturing", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("capturing", 0); err != nil {
		t.Fatal(err)
	}
	m.pendingCaptures.Store("capturing", struct{}{})
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("a terminal job with capture still pending must not be removed, removed %v", removed)
	}
	if ids, _ := m.store.List(); len(ids) != 1 || ids[0] != "capturing" {
		t.Fatalf("capturing job should survive the sweep, List = %v", ids)
	}
}

func TestGarbageCollectRereadsSentinelBeforeRemoval(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	// Old enough to collect, PID 0 so processAlive is false (the process has
	// exited). The first ExitCode read sees no sentinel; the re-read after the
	// liveness check sees the sentinel the supervisor wrote on its way out, so the
	// job must be kept this sweep rather than collected with its results unread.
	if _, err := m.store.Create(jobstore.Meta{ID: "racer", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.store = &reReadExitStore{jobStore: m.store, id: "racer"}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("a job that wrote its sentinel between GC's two checks must not be removed; removed %v", removed)
	}
	ids, err := m.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "racer" {
		t.Fatalf("racer should survive the sweep, List = %v", ids)
	}
}

func TestGarbageCollectRemovesExpired(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Create(jobstore.Meta{ID: "fresh", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("removed = %v, want [old]", removed)
	}
}

func TestGarbageCollectUntracksSettledCapture(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// The job's lazy capture settled before it aged out; its memo must not outlive
	// the job, or settledCapture would grow without bound in a long-running server.
	m.settleCapture("old")
	if !m.captureSettled("old") {
		t.Fatal("precondition: the job should be settled")
	}

	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("removed = %v, want [old]", removed)
	}
	if m.captureSettled("old") {
		t.Fatal("GarbageCollect must untrack a collected job's settled-capture memo")
	}
}

func TestGCInterval(t *testing.T) {
	cases := []struct{ ttl, want time.Duration }{
		{0, 0},                             // disabled
		{-time.Second, 0},                  // disabled
		{2 * time.Hour, time.Hour},         // ttl/2
		{4 * time.Minute, 2 * time.Minute}, // ttl/2, above the floor
		{time.Second, time.Minute},         // ttl/2 below the floor -> floored to 1m
	}
	for _, c := range cases {
		if got := gcInterval(c.ttl); got != c.want {
			t.Errorf("gcInterval(%v) = %v, want %v", c.ttl, got, c.want)
		}
	}
}

func TestRunPeriodicGCCollectsAndStops(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel() // stop the goroutine even if an assertion below fails early
	done := make(chan struct{})
	go func() { m.runPeriodicGC(ctx, 10*time.Millisecond); close(done) }()

	// The ticker collects the expired job.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ids, _ := m.store.List()
		if len(ids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic GC did not collect the expired job")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cancelling the context stops the loop and returns.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodicGC did not return after context cancellation")
	}
}

func TestGarbageCollectDisabledWhenTTLZero(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4}) // JobTTL 0
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-100 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("TTL 0 should disable GC, removed %v", removed)
	}
}
