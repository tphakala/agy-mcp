package manager

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func TestStartJobPersistsMetaAndSpawns(t *testing.T) {
	state := t.TempDir()
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	// Inject a test-owned cache file so the fresh run's id capture does not read
	// the developer's real agy cache or leak a capture goroutine racing cleanup.
	m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")

	job, err := m.StartJob(StartRequest{Prompt: "review main.go", Model: "Gemini 3.1 Pro (High)"})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if job.ID == "" {
		t.Fatal("empty job id")
	}

	// meta.json must exist and contain the prompt and agy args.
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Prompt != "review main.go" {
		t.Errorf("prompt = %q", meta.Prompt)
	}
	if !hasArg(meta.Args, "--model", "Gemini 3.1 Pro (High)") {
		t.Errorf("args missing model: %v", meta.Args)
	}
	if !contains(meta.Args, "-p") || !contains(meta.Args, "--dangerously-skip-permissions") {
		t.Errorf("args missing required flags: %v", meta.Args)
	}

	// The fake supervisor writes out/exit_code; wait briefly for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.store.ExitCode(job.ID); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.store.ExitCode(job.ID); !ok {
		t.Fatal("supervisor did not write exit_code")
	}
}

// TestStartJobWiresConversationID covers the continue-a-conversation path that
// no other StartJob test drives: an explicit conversation id must be threaded
// into the returned job, the persisted meta, and the agy --conversation arg.
func TestStartJobWiresConversationID(t *testing.T) {
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		StateDir:       t.TempDir(),
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")

	const convID = "11111111-2222-3333-4444-555555555555"
	job, err := m.StartJob(StartRequest{Prompt: "follow up", ConversationID: convID, Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if job.ConversationID != convID {
		t.Errorf("returned conversation id = %q, want %q", job.ConversationID, convID)
	}
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ConversationID != convID {
		t.Errorf("meta conversation id = %q, want %q", meta.ConversationID, convID)
	}
	if !hasArg(meta.Args, "--conversation", convID) {
		t.Errorf("args missing --conversation %s: %v", convID, meta.Args)
	}
}

func TestStartJobCleansUpDirOnSpawnFailure(t *testing.T) {
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  filepath.Join(t.TempDir(), "nonexistent-supervisor"),
		StateDir:       t.TempDir(),
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")
	cwd := t.TempDir()

	_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "spawn supervisor") {
		t.Fatalf("StartJob error = %v, want a spawn-supervisor failure", err)
	}

	// The job directory created before the failed spawn must be removed, not left
	// orphaned for GarbageCollect to reap later.
	ids, lerr := m.store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(ids) != 0 {
		t.Fatalf("orphaned job dir left on disk after spawn failure: %v", ids)
	}

	// The gate slot/key must also be released: a second same-cwd run fails at spawn
	// again, rather than being refused by the gate ("conflicting job").
	_, err2 := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err2 == nil || !strings.Contains(err2.Error(), "spawn supervisor") {
		t.Fatalf("second run error = %v, want spawn-supervisor (gate slot leaked?)", err2)
	}
}

// TestStartJobNormalizesCwd verifies StartJob canonicalizes a trailing-slash
// (or otherwise non-canonical) cwd before it reaches the gate key and the
// persisted meta, so two "same dir" runs serialize and the agy cache lookup
// matches. Regression test for issue #24.
func TestStartJobNormalizesCwd(t *testing.T) {
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		StateDir:       t.TempDir(),
		DefaultTimeout: time.Minute,
		DefaultModel:   "Gemini 3.1 Pro (High)",
		MaxConcurrency: 4,
	}
	m := New(c)
	// Inject a temp cache file so the run does not read the real ~/.gemini cache,
	// and shorten the post-exit capture so no capture goroutine outlives the test.
	m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")
	m.captureBudget = 0

	job, err := m.StartJob(StartRequest{Prompt: "x", Cwd: dir + "/"})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Cwd != canonical {
		t.Errorf("meta.Cwd = %q, want canonical %q (trailing slash not normalized)", meta.Cwd, canonical)
	}
	// The model and timeout defaults must be resolved into meta (and the args),
	// not left stale: issue #24 asks to normalize all-or-none.
	if meta.Model != "Gemini 3.1 Pro (High)" {
		t.Errorf("meta.Model = %q, want the resolved default", meta.Model)
	}
	if meta.Timeout != time.Minute {
		t.Errorf("meta.Timeout = %v, want the resolved default", meta.Timeout)
	}
	if !hasArg(meta.Args, "--model", "Gemini 3.1 Pro (High)") {
		t.Errorf("args missing resolved default model: %v", meta.Args)
	}
	if !hasArg(meta.Args, "--print-timeout", time.Minute.String()) {
		t.Errorf("args missing resolved default timeout: %v", meta.Args)
	}
}

// TestStartJobSerializesEquivalentCwdSpellings proves the headline behavior of
// issue #24: two fresh runs whose cwd is spelled differently (here a trailing
// slash) collapse to one normalized gate key, so the second is refused while the
// first still holds it. Without normalization the two keys differ and the runs
// would not serialize, re-exposing the agy session-lock hang the gate prevents.
func TestStartJobSerializesEquivalentCwdSpellings(t *testing.T) {
	dir := t.TempDir()
	// A sleeping fake agy keeps the first supervisor alive, so its gate key stays
	// held while the second run is attempted.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", SleepSecs: 30})
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		StateDir:       t.TempDir(),
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")

	job1, err := m.StartJob(StartRequest{Prompt: "first", Cwd: dir})
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	// Reap the whole supervisor group (bash + the sleeping fake agy) promptly,
	// regardless of how the test exits; the fake supervisor does not forward
	// signals, so Cancel alone would leave the sleep running.
	t.Cleanup(func() {
		// Guard the PID: syscall.Kill(-0, ...) would signal the test runner's own
		// process group, and a negative target needs a real group leader.
		if meta, lerr := m.store.Load(job1.ID); lerr == nil && meta.PID > 0 {
			_ = syscall.Kill(-meta.PID, syscall.SIGKILL)
		}
	})

	// The same directory spelled with a trailing slash normalizes to the same
	// gate key, so the gate must refuse it while job1 holds the key. The cap (4)
	// is not the limiter here; the per-cwd key is.
	_, err = m.StartJob(StartRequest{Prompt: "second", Cwd: dir + "/"})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("second run error = %v, want a gate conflict (equivalent cwd spellings not serialized)", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func hasArg(ss []string, flag, val string) bool {
	for i := 0; i+1 < len(ss); i++ {
		if ss[i] == flag && ss[i+1] == val {
			return true
		}
	}
	return false
}
