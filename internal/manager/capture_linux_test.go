package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// A completed fresh run must report the conversation id agy created for it,
// captured by diffing the conversation cache against the pre-run snapshot.
func TestFreshRunCapturesConversationID(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	const newUUID = "11111111-2222-3333-4444-555555555555"

	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
			Out: "done", CachePath: cachePath, CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, newUUID),
		}),
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = cachePath

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: cwd})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if job.ConversationID != "" {
		t.Fatalf("fresh run should start with no conversation id, got %q", job.ConversationID)
	}

	st := waitForCapturedID(t, m, job.ID, 3*time.Second)
	if st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if st.ConversationID != newUUID {
		t.Fatalf("captured conversation id = %q, want %q", st.ConversationID, newUUID)
	}
	// The completion goroutine must persist the id, not just report it via Status:
	// a later reader (e.g. after the manager restarts) must see it on disk.
	reloaded, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.ConversationID != newUUID {
		t.Fatalf("persisted conversation id = %q, want %q", reloaded.ConversationID, newUUID)
	}
}

// A successful fresh run that creates no conversation (cache unchanged) must report
// an empty conversation id and, crucially, still release its gate key after the
// capture budget so a later same-cwd run is not blocked forever.
// waitForExitCode blocks until the job has written its exit_code, the supervisor's last
// write into the job dir. Its callers use a cache-less testutil.WriteFakeSupervisor, which creates no
// conversation, so no manager-side meta rewrite follows the exit; for them this is the
// final write. A test that returns while a supervisor is still writing into a t.TempDir
// StateDir races the TempDir RemoveAll cleanup.
func waitForExitCode(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.store.ExitCode(id); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never wrote exit_code", id)
}

func TestFreshRunNoConversationReleasesKey(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}), // writes out + exit 0, never touches the cache
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = cachePath
	m.captureBudget = 50 * time.Millisecond
	m.capturePoll = 10 * time.Millisecond

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: cwd})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// Wait for the job to finish; the id stays empty (no conversation was created).
	deadline := time.Now().Add(2 * time.Second)
	seenDone := false
	for time.Now().Before(deadline) {
		if st, _ := m.Status(job.ID); st.State == StateDone {
			if st.ConversationID != "" {
				t.Fatalf("expected empty conversation id, got %q", st.ConversationID)
			}
			seenDone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seenDone {
		t.Fatal("job never reached done state")
	}

	// The gate key must have been released after the capture budget: a second
	// same-cwd fresh run eventually succeeds.
	deadline = time.Now().Add(2 * time.Second)
	for {
		job2, err := m.StartJob(StartRequest{Prompt: "again", Cwd: cwd})
		if err == nil {
			// Wait for the second job's supervisor to finish before returning. It writes
			// into StateDir (a t.TempDir), and a still-running supervisor races the TempDir
			// RemoveAll cleanup ("directory not empty"). Waiting for exit removes the racer.
			waitForExitCode(t, m, job2.ID)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gate key was not released after a fresh run that created no conversation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// If the manager dies after a fresh run completes but before its completion
// goroutine captured the id, a later Status must capture it lazily from the cache.
func TestStatusLazilyCapturesConversationID(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	cwd := t.TempDir()
	const newUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// The cache already reflects the conversation agy created for this cwd.
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, newUUID)), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  "/bin/true",
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	})
	m.cacheFile = cachePath

	// A completed fresh run left on disk by a previous manager: no captured id, a
	// recorded pre-run snapshot (empty: the cwd had no conversation), done sentinel.
	meta := jobstore.Meta{
		ID:            "job-restart-1",
		Cwd:           cwd,
		CwdUUIDBefore: "",
		StartedAt:     time.Now().Add(-time.Minute),
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
		t.Fatal(err)
	}

	st, err := m.Status(meta.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if st.ConversationID != newUUID {
		t.Fatalf("lazily captured id = %q, want %q", st.ConversationID, newUUID)
	}
	// The capture must be persisted so a subsequent Status is consistent.
	reloaded, err := m.store.Load(meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.ConversationID != newUUID {
		t.Fatalf("persisted id = %q, want %q", reloaded.ConversationID, newUUID)
	}
}

// If the cache entry is unchanged from the pre-run snapshot (the run created no
// new conversation), lazy capture must NOT attribute the stale id to this run.
func TestStatusLazyCaptureNoOpWhenCacheUnchanged(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	cwd := t.TempDir()
	const stale = "11112222-3333-4444-5555-666677778888"
	// The cache holds an id that was already present before this run started.
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, stale)), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  "/bin/true",
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	})
	m.cacheFile = cachePath

	// A completed fresh run whose pre-run snapshot already equals the cache entry.
	meta := jobstore.Meta{
		ID:            "job-nochange",
		Cwd:           cwd,
		CwdUUIDBefore: stale,
		StartedAt:     time.Now().Add(-time.Minute),
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(meta.ID, 0); err != nil {
		t.Fatal(err)
	}

	st, err := m.Status(meta.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ConversationID != "" {
		t.Fatalf("must not attribute the unchanged stale id, got %q", st.ConversationID)
	}
}

// A fresh run started while the conversation cache is unreadable must disable
// capture for that run: even when the cache later becomes readable with a new
// entry, the id cannot be attributed to this run.
func TestFreshRunWithCorruptCacheDisablesCapture(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()

	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = cachePath
	m.captureBudget = 50 * time.Millisecond
	m.capturePoll = 10 * time.Millisecond

	job, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: cwd})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !meta.CaptureDisabled {
		t.Fatal("expected CaptureDisabled for a run started with an unreadable cache")
	}
	waitForExitCode(t, m, job.ID)

	// The cache "recovers" with an entry for this cwd; it must not be captured.
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, "aaaa1111-2222-3333-4444-555566667777")), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(job.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ConversationID != "" {
		t.Fatalf("capture-disabled run must report no id, got %q", st.ConversationID)
	}
}

func waitForCapturedID(t *testing.T, m *Manager, id string, within time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	var st Status
	for time.Now().Before(deadline) {
		var err error
		st, err = m.Status(id)
		if err == nil && st.State == StateDone && st.ConversationID != "" {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	return st
}
