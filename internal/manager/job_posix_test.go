//go:build linux || darwin

package manager

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

func TestStartJobPersistsMetaAndSpawns(t *testing.T) {
	// withCacheFile injects a test-owned cache file so nothing here can read the
	// developer's real agy cache. This run resolves no conversation (no
	// continue_latest), so the file is never actually read; the injection is
	// insurance against a future change here, not a dependency of this test.
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})

	job, err := m.StartJob(StartRequest{Prompt: "review main.go", Model: "gemini-3.1-pro-high"})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	// The fake supervisor writes out/exit_code; wait for it before teardown so its
	// writes finish before t.TempDir RemoveAll (see deferJobDone).
	deferJobDone(t, m, job.ID)
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
	if !hasArg(meta.Args, "--model", "gemini-3.1-pro-high") {
		t.Errorf("args missing model: %v", meta.Args)
	}
	if !contains(meta.Args, "-p") || !contains(meta.Args, "--dangerously-skip-permissions") {
		t.Errorf("args missing required flags: %v", meta.Args)
	}
}

// TestStartJobWiresConversationID covers the continue-a-conversation path that
// no other StartJob test drives: an explicit conversation id must be threaded
// into the returned job, the persisted meta, and the agy --conversation arg.
func TestStartJobWiresConversationID(t *testing.T) {
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})

	const convID = "11111111-2222-3333-4444-555555555555"
	job, err := m.StartJob(StartRequest{Prompt: "follow up", ConversationID: convID, Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	deferJobDone(t, m, job.ID)
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

func TestStartJobIdempotencyReusesExistingJob(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "done", Sleep: 30 * time.Second})
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	cwd := t.TempDir()
	req := StartRequest{Prompt: "review", Cwd: cwd, IdempotencyKey: "retry-1"}

	first, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	killJob(t, m, first.ID)
	second, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("retry StartJob: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("retry created job %s, want existing %s", second.ID, first.ID)
	}
	ids, err := m.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("job ids = %v, want exactly one", ids)
	}
	meta, err := m.store.Load(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IdempotencyKey != "retry-1" {
		t.Fatalf("persisted idempotency_key = %q, want retry-1", meta.IdempotencyKey)
	}
}

func TestStartJobIdempotencyReplaysTerminalState(t *testing.T) {
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	req := StartRequest{Prompt: "review", Cwd: t.TempDir(), IdempotencyKey: "retry-done"}
	first, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	testutil.WaitFor(t, 15*time.Second, func() bool {
		_, ok := m.store.ExitCode(first.ID)
		return ok
	}, "first idempotent job did not finish")

	replay, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("terminal replay StartJob: %v", err)
	}
	if replay.ID != first.ID || replay.State != StateDone {
		t.Fatalf("terminal replay = %+v, want same job %s in state done", replay, first.ID)
	}
}

func TestStartJobIdempotencyFailsClosedWhileCreatorHoldsClaim(t *testing.T) {
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	ok, err := m.acquireIdempotency("retry-starting")
	if err != nil || !ok {
		t.Fatalf("pre-acquire idempotency claim = (%v, %v), want (true, nil)", ok, err)
	}
	defer m.releaseIdempotency("retry-starting")

	_, err = m.StartJob(StartRequest{Prompt: "review", Cwd: t.TempDir(), IdempotencyKey: "retry-starting"})
	if err == nil || !strings.Contains(err.Error(), "is starting; retry with the same key") {
		t.Fatalf("StartJob during creation claim error = %v, want retry-safe starting refusal", err)
	}
	ids, lerr := m.store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(ids) != 0 {
		t.Fatalf("refused retry created jobs: %v", ids)
	}
}

func TestStartJobIdempotencyRejectsDifferentRequest(t *testing.T) {
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	cwd := t.TempDir()
	first, err := m.StartJob(StartRequest{Prompt: "one", Cwd: cwd, IdempotencyKey: "retry-1"})
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	deferJobDone(t, m, first.ID)
	_, err = m.StartJob(StartRequest{Prompt: "two", Cwd: cwd, IdempotencyKey: "retry-1"})
	if err == nil || !strings.Contains(err.Error(), "different normalized request") {
		t.Fatalf("mismatched retry error = %v, want fail-closed idempotency rejection", err)
	}
	ids, lerr := m.store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(ids) != 1 {
		t.Fatalf("mismatched retry created another job: %v", ids)
	}
}

// TestStartJobIdempotencySkipsFailedCreationRemnant pins the PID==0 guard in
// findIdempotentJob: a record bound to the key that never persisted a supervisor
// PID is a failed-creation remnant (what an ignored store.Remove error during
// teardown leaves behind), so a retry must start a fresh job rather than replay a
// run that never started.
func TestStartJobIdempotencySkipsFailedCreationRemnant(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "done", Sleep: 30 * time.Second})
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	req := StartRequest{Prompt: "review", Cwd: t.TempDir(), IdempotencyKey: "retry-orphan"}

	// Seed the remnant with the SAME normalized cwd+args StartJob will derive, so it
	// gets past the different-request check and exercises the PID==0 skip. No PID and
	// no exit sentinel is exactly the leaked-record state.
	nreq, err := m.normalizeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Create(jobstore.Meta{
		ID:             "orphan",
		IdempotencyKey: req.IdempotencyKey,
		Cwd:            nreq.Cwd,
		Args:           buildAgyArgs(nreq),
		Prompt:         nreq.Prompt,
		StartedAt:      time.Now().UTC(),
		BootID:         readBootID(),
	}); err != nil {
		t.Fatal(err)
	}

	job, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if job.ID == "orphan" {
		t.Fatal("replayed the failed-creation remnant instead of starting a fresh job")
	}
	killJob(t, m, job.ID)
}

func TestStartJobCleansUpDirOnSpawnFailure(t *testing.T) {
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  filepath.Join(t.TempDir(), "nonexistent-supervisor"),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
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
	// withCacheFile injects a temp cache file so continue_latest does not read
	// the real ~/.gemini cache.
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		defaultTimeout: time.Minute,
		defaultModel:   "gemini-3.1-pro-high",
		maxConcurrency: 4,
		withCacheFile:  true,
	})

	job, err := m.StartJob(StartRequest{Prompt: "x", Cwd: dir + "/"})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	deferJobDone(t, m, job.ID)
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Cwd != canonical {
		t.Errorf("meta.Cwd = %q, want canonical %q (trailing slash not normalized)", meta.Cwd, canonical)
	}
	// The model and timeout defaults must be resolved into meta (and the args),
	// not left stale: issue #24 asks to normalize all-or-none.
	if meta.Model != "gemini-3.1-pro-high" {
		t.Errorf("meta.Model = %q, want the resolved default", meta.Model)
	}
	if meta.Timeout != time.Minute {
		t.Errorf("meta.Timeout = %v, want the resolved default", meta.Timeout)
	}
	if !hasArg(meta.Args, "--model", "gemini-3.1-pro-high") {
		t.Errorf("args missing resolved default model: %v", meta.Args)
	}
	if !hasArg(meta.Args, "--print-timeout", time.Minute.String()) {
		t.Errorf("args missing resolved default timeout: %v", meta.Args)
	}
}

// TestStartJobReducesModelRowToID: a model value that arrives as a whole
// `agy models` row ("<id>\t<label>") is a string agy does not accept as --model,
// so StartJob must reduce it to the id column before it reaches the args and the
// persisted meta (issue #135). The reduction runs after the default fallback is
// applied, so it must hold whether the row comes from the caller's request or
// from AGY_MCP_DEFAULT_MODEL; both are covered here.
func TestStartJobReducesModelRowToID(t *testing.T) {
	const row = "gemini-3.1-pro-high\tGemini 3.1 Pro (High)"
	for _, tc := range []struct {
		name          string
		requestModel  string // the request's Model
		requestEffort string // set only where effort coexistence is asserted
		defaultModel  string // the configured AGY_MCP_DEFAULT_MODEL
		assertEffort  bool
	}{
		{
			// The request carries the row while a DIFFERENT default is configured, so
			// this also pins that an explicit request model wins: the reduction runs
			// after the fallback, and nothing else in the suite exercises both being
			// present at once. Effort is set so the reduction is exercised on the
			// request shape that motivated it; buildAgyArgs emits --effort independently
			// of the model, so asserting both just pins that they coexist, not that agy
			// would accept the pair.
			name:          "explicit request row wins over a different default",
			requestModel:  row,
			requestEffort: "high",
			defaultModel:  "gemini-3.6-flash-low",
			assertEffort:  true,
		},
		{
			// The row arrives via the fallback applied inside StartJob, which is
			// invisible to the tool boundary that validates the request, so the
			// reduction must cover it there too.
			name:         "default fallback row",
			defaultModel: row,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{
				agyPath:        "/usr/bin/agy",
				supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
				defaultModel:   tc.defaultModel,
				defaultTimeout: time.Minute,
				maxConcurrency: 4,
				withCacheFile:  true,
			})

			job, err := m.StartJob(StartRequest{Prompt: "x", Model: tc.requestModel, Effort: tc.requestEffort})
			if err != nil {
				t.Fatalf("StartJob: %v", err)
			}
			deferJobDone(t, m, job.ID)
			meta, err := m.store.Load(job.ID)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if meta.Model != "gemini-3.1-pro-high" {
				t.Errorf("meta.Model = %q, want the id column alone", meta.Model)
			}
			if !hasArg(meta.Args, "--model", "gemini-3.1-pro-high") {
				t.Errorf("args must carry the id alone, got: %v", meta.Args)
			}
			if tc.assertEffort && !hasArg(meta.Args, "--effort", tc.requestEffort) {
				t.Errorf("args missing effort: %v", meta.Args)
			}
		})
	}
}

// TestStartJobRunsFreshSameCwdRunsConcurrently pins the behaviour change that
// came with reading the conversation id from agy's own stream: fresh runs no
// longer key the gate on their cwd, so two of them in one directory (here spelled
// differently, to also cover normalization) both start instead of the second
// being refused. Only a resolved conversation id serializes now, because that is
// the constraint agy itself imposes.
func TestStartJobRunsFreshSameCwdRunsConcurrently(t *testing.T) {
	dir := t.TempDir()
	// A sleeping fake agy keeps the first supervisor alive, so its gate key stays
	// held while the second run is attempted.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Sleep: 30 * time.Second})
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})

	job1, err := m.StartJob(StartRequest{Prompt: "first", Cwd: dir})
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	// Reap the whole supervisor group (bash + the sleeping fake agy) promptly,
	// regardless of how the test exits; the fake supervisor does not forward
	// signals, so Cancel alone would leave the sleep running.
	killJob(t, m, job1.ID)

	job2, err := m.StartJob(StartRequest{Prompt: "second", Cwd: dir + "/"})
	if err != nil {
		t.Fatalf("second fresh run in the same cwd must start, got: %v", err)
	}
	if job2.ID == job1.ID {
		t.Fatal("the two runs must be distinct jobs")
	}
	killJob(t, m, job2.ID)
}

// TestStartJobSerializesSameConversation pins what the gate still enforces: two
// runs continuing one conversation cannot overlap, because concurrent agy
// sessions on the same conversation trigger its session-lock hang.
func TestStartJobSerializesSameConversation(t *testing.T) {
	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Sleep: 30 * time.Second})
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})

	const conv = "shared-conversation-id"
	job1, err := m.StartJob(StartRequest{Prompt: "first", Cwd: dir, ConversationID: conv})
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	killJob(t, m, job1.ID)

	_, err = m.StartJob(StartRequest{Prompt: "second", Cwd: dir, ConversationID: conv})
	if err == nil || !strings.Contains(err.Error(), "already running on conversation") {
		t.Fatalf("second run error = %v, want a refusal naming the shared conversation", err)
	}
}

// deferJobDone registers a cleanup that blocks until the job's supervisor has
// written its exit-code sentinel, which WriteFakeSupervisor emits as its LAST
// write to the job dir (see fakesupervisor.go). A StartJob test that spawns a
// supervisor and returns before it finishes races the detached supervisor's
// writes against t.TempDir teardown, so RemoveAll intermittently fails with
// "directory not empty" (issue #149).
//
// It is registered right after a successful StartJob rather than called at the
// end of the test, so it still runs (t.Cleanup is LIFO, so before the newManager
// StateDir's own teardown) when an assertion fails early via t.Fatalf and skips
// the rest of the body; calling it inline at the end would be skipped on exactly
// that path and mask the real failure with a spurious RemoveAll error.
//
// Only tests whose fake supervisor actually writes into the job dir and completes
// on its own need it. Tests that expect StartJob to fail (no supervisor spawned),
// that kill a sleeping supervisor (killJob), or that use a non-writing supervisor
// such as `sleep` are not exposed; a supervisor that is SIGKILLed never writes an
// exit code, so waiting for one there would time out.
func deferJobDone(t *testing.T, m *Manager, id string) {
	t.Helper()
	t.Cleanup(func() {
		testutil.WaitFor(t, 15*time.Second, func() bool {
			_, ok := m.store.ExitCode(id)
			return ok
		}, "supervisor did not write its exit-code sentinel")
	})
}

func contains(ss []string, want string) bool {
	return slices.Contains(ss, want)
}

func hasArg(ss []string, flag, val string) bool {
	for i := 0; i+1 < len(ss); i++ {
		if ss[i] == flag && ss[i+1] == val {
			return true
		}
	}
	return false
}
