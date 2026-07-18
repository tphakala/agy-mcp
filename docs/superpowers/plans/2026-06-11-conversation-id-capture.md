# Conversation ID Capture Reliability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix GitHub issue tphakala/agy-mcp#23: agy_run_sync must not return `done` without a conversation id for a fresh run, lazy capture must not misattribute another run's conversation, restored jobs must capture like normal ones, and torn cache reads must be detected instead of swallowed.

**Architecture:** agy-mcp captures conversation ids by diffing agy's `last_conversations.json` cache (keyed by cwd) against a pre-run snapshot, because agy mints its own UUID for fresh runs. The cache is flushed by a separate agy daemon that can lag the process exit, so the manager's completion goroutine retries the diff for a bounded budget while holding the run's gate key. This plan (a) makes the test fixture reproduce the real-world ordering (exit sentinel first, cache later), (b) introduces an explicit "capture pending" state on the Manager that agy_run_sync polls through instead of returning early, (c) mirrors the capture in the restored-job watcher, (d) makes cache reads error-aware with a per-run `CaptureDisabled` flag when the pre-run snapshot is unreadable, and (e) guards the lazy (post-restart) capture against attributing a later same-cwd run's id, with a permanent "settled" memo once a capture can no longer succeed.

**Tech Stack:** Go 1.26, stdlib only (`sync`, `time`, `encoding/json`), MCP go-sdk for the tool layer, bash-script test fakes in `internal/testutil`. Job supervision tests are Linux-only (the suite already assumes this).

**Branch:** create `fix/conversation-id-capture` off `main` before Task 1:

```bash
git -C /home/thakala/src/agy-mcp switch -c fix/conversation-id-capture
```

All paths below are relative to `/home/thakala/src/agy-mcp`.

---

## File Structure

| File | Change |
|---|---|
| `internal/testutil/fakesupervisor.go` | Write `exit_code` before the cache; new `CacheDelay` field to simulate the cache-daemon lag |
| `internal/manager/conversation.go` | `loadCache` returns errors; `snapshotCwd` gains an ok result; retry helper |
| `internal/jobstore/store.go` | New `Meta.CaptureDisabled` field |
| `internal/config/config.go` | New `Config.ConversationCacheFile` override (test seam) |
| `internal/manager/manager.go` | Pending-capture tracking, `CapturePending`, `CaptureDisabled` handling in StartJob, watchRestored capture, lazy-capture guards + settle memo |
| `internal/mcptools/run_sync.go` | Poll through a pending capture instead of returning done-with-no-id |
| `internal/manager/conversation_test.go` | Update for new signatures; new loadCache unit tests |
| `internal/manager/capture_linux_test.go` | New tests: CaptureDisabled, CapturePending lifecycle, later-run guard, settle horizon |
| `internal/manager/restore_linux_test.go` | New test: watcher captures id for restored fresh run |
| `internal/mcptools/run_sync_test.go` | newTestManager gets a real cache; new late-cache regression test |

---

### Task 1: Test fixture realism: exit sentinel before cache, optional cache lag

The fake supervisor currently writes the cache payload BEFORE `exit_code`, the inverse of reality (the supervisor writes `exit_code` the moment agy exits; agy's cache daemon flushes the cache around or after that). This ordering is what masks the run_sync bug. Flip it, and add a `CacheDelay` knob that writes the cache from a backgrounded subshell after the script exits, which is exactly the real daemon-lag shape.

**Files:**
- Modify: `internal/testutil/fakesupervisor.go`

- [ ] **Step 1: Update the FakeSupervisor struct and doc comments**

In `internal/testutil/fakesupervisor.go`, replace the `CachePath`/`CacheJSON` field comments and add `CacheDelay`. Replace:

```go
	// CachePath, when set, makes the script write CacheJSON to that path
	// before exit_code, mimicking agy persisting its conversation cache.
	CachePath string
	// CacheJSON is the cache payload to write; requires CachePath.
	CacheJSON string
}
```

with:

```go
	// CachePath, when set, makes the script write CacheJSON to that path
	// after exit_code, mimicking the real ordering: the supervisor writes the
	// exit sentinel at process exit, and agy's cache daemon flushes the
	// conversation cache around or after that moment.
	CachePath string
	// CacheJSON is the cache payload to write; requires CachePath.
	CacheJSON string
	// CacheDelay, when positive, writes the cache from a backgrounded subshell
	// that sleeps this long first, so the cache lands only after the script
	// (and its exit_code) is gone. This reproduces the cache-daemon lag that
	// the manager's capture retry exists for. Requires CachePath.
	CacheDelay time.Duration
}
```

Add `"time"` to the imports.

Then update the WriteFakeSupervisor doc comment: replace the sentence

```go
// shell quoting. exit_code is always written last because the manager treats
// its presence as job completion.
```

with:

```go
// shell quoting. exit_code is written before the cache payload, matching the
// real ordering the manager must tolerate: job completion (the sentinel) can
// be observable before the conversation cache has been flushed.
```

- [ ] **Step 2: Reorder the script generation and implement CacheDelay**

In `WriteFakeSupervisor`, add the validation right after the existing CacheJSON check:

```go
	if cfg.CacheDelay > 0 && cfg.CachePath == "" {
		t.Fatal("FakeSupervisor: CacheDelay requires CachePath")
	}
```

Then replace the script-assembly block:

```go
	if cfg.CachePath != "" {
		cachePayload := filepath.Join(dir, "cache-payload.json")
		if err := os.WriteFile(cachePayload, []byte(cfg.CacheJSON), 0o644); err != nil {
			t.Fatalf("write fake supervisor cache payload: %v", err)
		}
		fmt.Fprintf(&sb, "cat %q > %q\n", cachePayload, cfg.CachePath)
	}
	sb.WriteString("printf '%s' \"$code\" > \"$dir/exit_code\"\n")
```

with:

```go
	sb.WriteString("printf '%s' \"$code\" > \"$dir/exit_code\"\n")
	if cfg.CachePath != "" {
		cachePayload := filepath.Join(dir, "cache-payload.json")
		if err := os.WriteFile(cachePayload, []byte(cfg.CacheJSON), 0o644); err != nil {
			t.Fatalf("write fake supervisor cache payload: %v", err)
		}
		if cfg.CacheDelay > 0 {
			// Backgrounded so the script (the fake supervisor process) exits
			// first and the cache lands later, like agy's real cache daemon.
			fmt.Fprintf(&sb, "( sleep %.3f; cat %q > %q ) &\n",
				cfg.CacheDelay.Seconds(), cachePayload, cfg.CachePath)
		} else {
			fmt.Fprintf(&sb, "cat %q > %q\n", cachePayload, cfg.CachePath)
		}
	}
```

- [ ] **Step 3: Run the affected packages**

Run: `go test ./internal/testutil/... ./internal/manager/... ./internal/mcptools/... 2>&1 | tail -20`
Expected: all PASS. `TestFreshRunCapturesConversationID` still passes because the manager's completion goroutine retries the capture for its 2s budget, which now genuinely exercises the retry. `TestFakeSupervisorWritesCache` still passes because it inspects files only after the script has exited, when both exist regardless of order.

- [ ] **Step 4: Commit**

```bash
git add internal/testutil/fakesupervisor.go
git commit -m "test: fake supervisor writes exit_code before the conversation cache

Matches the real ordering (sentinel at process exit, cache flushed by agy's
daemon afterwards) and adds CacheDelay to simulate the daemon lag. The old
inverted ordering masked the agy_run_sync done-without-id bug."
```

---

### Task 2: Error-aware cache reads and CaptureDisabled

`loadCache` currently swallows read and parse errors, so a torn read (agy rewrites the file with O_TRUNC and no lock) looks identical to "no entry". That silently degrades `continue_latest` and, when it hits the pre-run snapshot, sets up a misattribution: a snapshot that reads as "" makes a pre-existing conversation look newly created. Make errors visible, retry once (torn reads are transient), and when the snapshot still cannot be read, disable capture for that run via a persisted meta flag.

**Files:**
- Modify: `internal/manager/conversation.go` (full rewrite below)
- Modify: `internal/jobstore/store.go` (one field)
- Modify: `internal/manager/manager.go` (StartJob snapshot block, captureFreshConversationID guard, lazyCaptureConversationID guard)
- Modify: `internal/manager/conversation_test.go`
- Test: `internal/manager/conversation_test.go`, `internal/manager/capture_linux_test.go`

- [ ] **Step 1: Write the failing unit tests**

Append to `internal/manager/conversation_test.go`:

```go
func TestLoadCacheMissingFileIsEmptyNotError(t *testing.T) {
	cache, err := loadCache(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing cache is a normal empty cache, got error: %v", err)
	}
	if len(cache) != 0 {
		t.Fatalf("want empty cache, got %v", cache)
	}
}

func TestLoadCacheTornReadIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCache(p); err == nil {
		t.Fatal("a torn/unparsable cache read must surface an error")
	}
}

func TestResolveLatestNotFoundOnTornCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveLatest(p, "/w"); ok {
		t.Fatal("a torn cache must resolve to not-found, not to a bogus id")
	}
}

func TestSnapshotCwdUnknownOnTornCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshotCwd(p, "/w"); ok {
		t.Fatal("a torn cache must report the snapshot as unknown")
	}
}

func TestSnapshotCwdOkOnMissingCache(t *testing.T) {
	before, ok := snapshotCwd(filepath.Join(t.TempDir(), "absent.json"), "/w")
	if !ok || before != "" {
		t.Fatalf("missing cache = valid empty snapshot, got %q,%v", before, ok)
	}
}
```

Also update the existing `TestCaptureNewUUIDByDiff` for the new two-value `snapshotCwd`:

```go
	before, ok := snapshotCwd(cache, "/w") // "old"
	if !ok {
		t.Fatal("snapshot should be readable")
	}
```

(and keep using `before` below it as today).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/manager/ -run 'TestLoadCache|TestResolveLatest|TestSnapshotCwd|TestCaptureNewUUID' 2>&1 | tail -5`
Expected: FAIL (compile errors: `loadCache` returns one value, `snapshotCwd` returns one value).

- [ ] **Step 3: Rewrite conversation.go**

Replace the entire content of `internal/manager/conversation.go` with:

```go
package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// loadCache reads agy's conversation cache. A missing file is a normal empty
// cache (fresh agy install, no conversations yet). A read or parse failure is
// reported: agy rewrites the file in place (O_TRUNC, no lock), so a concurrent
// read can be torn, and callers must not mistake a torn read for "no entry"
// when the difference matters.
func loadCache(cacheFile string) (map[string]string, error) {
	b, err := os.ReadFile(cacheFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse conversation cache %s: %w", cacheFile, err)
	}
	return raw, nil
}

// loadCacheRetry reads the cache, retrying once on failure: torn reads are
// transient (agy's rewrite completes in microseconds), so an immediate re-read
// usually observes a complete file.
func loadCacheRetry(cacheFile string) (map[string]string, error) {
	cache, err := loadCache(cacheFile)
	if err == nil {
		return cache, nil
	}
	return loadCache(cacheFile)
}

// resolveLatest returns the most recent conversation UUID for cwd, if any. An
// unreadable cache resolves to not-found: continue_latest then starts a fresh
// conversation, the documented fallback for "no prior conversation".
func resolveLatest(cacheFile, cwd string) (string, bool) {
	cache, err := loadCacheRetry(cacheFile)
	if err != nil {
		return "", false
	}
	id, ok := cache[cwd]
	return id, ok && id != ""
}

// snapshotCwd records the cwd's UUID before a run. ok=false means the snapshot
// could not be taken (torn or corrupt cache even after a retry); the caller
// must disable capture for the run rather than guess, because an empty-by-error
// snapshot would make a pre-existing conversation look newly created.
func snapshotCwd(cacheFile, cwd string) (string, bool) {
	cache, err := loadCacheRetry(cacheFile)
	if err != nil {
		return "", false
	}
	return cache[cwd], true
}

// captureNewUUID returns the cwd's UUID after a run, but only if it changed
// from the pre-run snapshot. A failed run leaves the cache untouched, and a
// torn read yields no capture, so a single call never misattributes an old
// conversation to this run. Both call sites sit in retry loops, so no internal
// retry is needed here.
func captureNewUUID(cacheFile, cwd, before string) (string, bool) {
	cache, err := loadCache(cacheFile)
	if err != nil {
		return "", false
	}
	after := cache[cwd]
	if after != "" && after != before {
		return after, true
	}
	return "", false
}
```

- [ ] **Step 4: Add the Meta field**

In `internal/jobstore/store.go`, after the `CwdUUIDBefore` field, add:

```go
	// CaptureDisabled marks a fresh run whose pre-run cache snapshot could not
	// be read: without a trustworthy snapshot a post-run cache diff cannot be
	// attributed safely, so conversation-id capture is skipped for this job.
	CaptureDisabled bool `json:"capture_disabled,omitempty"`
```

- [ ] **Step 5: Wire CaptureDisabled through the manager**

In `internal/manager/manager.go`:

(a) Replace the StartJob snapshot block:

```go
	if req.ConversationID == "" {
		meta.CwdUUIDBefore = snapshotCwd(m.cacheFile, cwd)
	}
```

with:

```go
	if req.ConversationID == "" {
		if before, ok := snapshotCwd(m.cacheFile, cwd); ok {
			meta.CwdUUIDBefore = before
		} else {
			// No trustworthy pre-run snapshot: a post-run diff could attribute
			// a pre-existing conversation to this run. Report no id instead.
			meta.CaptureDisabled = true
			log.Printf("agy-mcp: job %s: conversation cache unreadable; id capture disabled for this run", id)
		}
	}
```

(b) In `captureFreshConversationID`, replace the first guard:

```go
	if meta.ConversationID != "" {
		return
	}
```

with:

```go
	if meta.ConversationID != "" || meta.CaptureDisabled {
		return
	}
```

(c) In `lazyCaptureConversationID`, after the known-id guard, add:

```go
	if meta.CaptureDisabled {
		return ""
	}
```

- [ ] **Step 6: Run unit tests**

Run: `go test ./internal/manager/ -run 'TestLoadCache|TestResolveLatest|TestSnapshotCwd|TestCaptureNewUUID|TestCaptureNoChange' -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 7: Write the failing end-to-end CaptureDisabled test**

Append to `internal/manager/capture_linux_test.go`:

```go
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
```

- [ ] **Step 8: Run it (it should already pass; verify it tests the right thing)**

Run: `go test ./internal/manager/ -run TestFreshRunWithCorruptCacheDisablesCapture -v 2>&1 | tail -5`
Expected: PASS. To prove the test bites, temporarily revert step 5(c) (remove the `meta.CaptureDisabled` guard in lazyCaptureConversationID), rerun, and confirm it FAILS with `capture-disabled run must report no id`; then restore the guard.

- [ ] **Step 9: Run the full suite and commit**

Run: `go test ./... 2>&1 | tail -10`
Expected: all PASS.

```bash
git add internal/manager/conversation.go internal/manager/conversation_test.go internal/manager/manager.go internal/manager/capture_linux_test.go internal/jobstore/store.go
git commit -m "fix: detect torn conversation-cache reads instead of swallowing them

loadCache now reports read/parse errors (a missing file stays a normal empty
cache), resolveLatest retries once and falls through to fresh-run semantics,
and an unreadable pre-run snapshot disables capture for that run via a new
Meta.CaptureDisabled flag, closing the torn-snapshot misattribution edge."
```

---

### Task 3: Config.ConversationCacheFile

The mcptools tests construct managers via `manager.New(config.Config{...})` and cannot reach the unexported `cacheFile` field, so today they silently read the real `~/.gemini` cache. A config override fixes that and is the seam Task 4's tests need.

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/manager/manager.go` (New)
- Test: `internal/manager/conversation_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/manager/conversation_test.go`:

```go
func TestNewUsesConfiguredCacheFile(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 1,
		ConversationCacheFile: "/tmp/agy-test-cache.json"})
	if m.cacheFile != "/tmp/agy-test-cache.json" {
		t.Fatalf("cacheFile = %q, want the configured override", m.cacheFile)
	}
}
```

Add `"github.com/tphakala/agy-mcp/internal/config"` to that file's imports.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/manager/ -run TestNewUsesConfiguredCacheFile 2>&1 | tail -5`
Expected: FAIL (compile error: unknown field ConversationCacheFile).

- [ ] **Step 3: Add the config field**

In `internal/config/config.go`, add to the `Config` struct (after the existing fields):

```go
	// ConversationCacheFile overrides where agy's conversation cache
	// (last_conversations.json) is read from. Empty means agy's default
	// location under the user's home. Primarily a test seam.
	ConversationCacheFile string
```

`Resolve()` is unchanged (the zero value selects the default).

- [ ] **Step 4: Use it in manager.New**

In `internal/manager/manager.go`, replace inside `New`:

```go
		cacheFile:            agyCachePath(),
```

with:

```go
		cacheFile:            cmp.Or(c.ConversationCacheFile, agyCachePath()),
```

and add `"cmp"` to the imports.

- [ ] **Step 5: Run and commit**

Run: `go test ./internal/manager/ ./internal/config/ 2>&1 | tail -5`
Expected: PASS.

```bash
git add internal/config/config.go internal/manager/manager.go internal/manager/conversation_test.go
git commit -m "feat: allow overriding the agy conversation cache path via config"
```

---

### Task 4: CapturePending and the agy_run_sync grace

The core fix. The Manager tracks which jobs have a fresh-run capture "armed but not settled"; agy_run_sync keeps polling while a done job's capture is still pending instead of returning done-with-no-id. The bound is the manager's own captureBudget (the completion goroutine always clears the pending mark), so no new timeout constant is needed in mcptools.

**Files:**
- Modify: `internal/manager/manager.go`
- Modify: `internal/mcptools/run_sync.go`
- Modify: `internal/mcptools/run_sync_test.go`
- Test: `internal/manager/capture_linux_test.go`, `internal/mcptools/run_sync_test.go`

- [ ] **Step 1: Write the failing mcptools regression test**

First rewire `newTestManager` in `internal/mcptools/run_sync_test.go` so fake runs produce a conversation id through a test-owned cache (this also stops these tests from reading the real `~/.gemini` cache). Replace the whole function with:

```go
// testConversationID is the id the fake supervisor's cache write attributes to
// fresh runs started by newTestManager.
const testConversationID = "abcdabcd-1234-5678-9abc-def012345678"

// newTestManager builds a manager around a fake agy and fake supervisor. The
// fake supervisor writes a conversation cache entry for the test's cwd after
// the exit sentinel, so fresh runs capture an id the way real runs do, against
// a test-owned cache file rather than the real agy cache.
func newTestManager(t *testing.T, fake testutil.FakeAgy) (mgr *manager.Manager, stateDir string) {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:   agy,
		CachePath: cachePath,
		CacheJSON: fmt.Sprintf(`{%q:%q}`, cwd, testConversationID),
	})
	stateDir = t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	return manager.New(c), stateDir
}
```

Add `"fmt"` to the file's imports. Then append the regression test:

```go
// A fresh run whose conversation cache lands only after the exit sentinel (the
// real-world ordering: agy's cache daemon flushes after the process exits) must
// still return its conversation id from agy_run_sync. Returning done with no id
// loses the id for good, because a sync caller has no reason to poll again.
func TestAgyRunSyncReturnsLateCapturedConversationID(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "OK", Exit: 0})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const uuid = "12121212-3434-5656-7878-909090909090"
	sup := testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
		AgyPath:    agy,
		CachePath:  cachePath,
		CacheJSON:  fmt.Sprintf(`{%q:%q}`, cwd, uuid),
		CacheDelay: 700 * time.Millisecond,
	})
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: t.TempDir(),
		DefaultTimeout: time.Minute, MaxConcurrency: 4,
		ConversationCacheFile: cachePath}
	mgr := manager.New(c)
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}
	if sc["conversation_id"] != uuid {
		t.Fatalf("conversation_id = %v, want %q (the id must not be lost to cache-flush lag)",
			sc["conversation_id"], uuid)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/mcptools/ -run TestAgyRunSyncReturnsLateCapturedConversationID -v 2>&1 | tail -5`
Expected: FAIL on the conversation_id assertion (`conversation_id = <nil>, want "12121212-..."`). The exit sentinel appears immediately, run_sync's first poll returns done with no id, and the cache lands 700ms too late. (It fails to compile until CapturePending exists only if you wrote the run_sync change first; at this point nothing references it yet, so it fails at the assertion.)

- [ ] **Step 3: Add pending-capture tracking to the Manager**

In `internal/manager/manager.go`:

(a) Add to the Manager struct, after `cacheFile`:

```go
	// pendingCaptures holds the job ids of fresh runs whose conversation-id
	// capture is armed but not yet settled (the post-exit capture attempt has
	// not finished). Keyed by job id; values are struct{}.
	pendingCaptures sync.Map
```

Add `"sync"` to the imports.

(b) In StartJob, arm the capture in the snapshot-ok branch (final form of the Task 2 block):

```go
	if req.ConversationID == "" {
		if before, ok := snapshotCwd(m.cacheFile, cwd); ok {
			meta.CwdUUIDBefore = before
			m.pendingCaptures.Store(id, struct{}{})
		} else {
			// No trustworthy pre-run snapshot: a post-run diff could attribute
			// a pre-existing conversation to this run. Report no id instead.
			meta.CaptureDisabled = true
			log.Printf("agy-mcp: job %s: conversation cache unreadable; id capture disabled for this run", id)
		}
	}
```

(c) Disarm on every StartJob failure path. After `m.store.Create` fails:

```go
	dir, err := m.store.Create(meta)
	if err != nil {
		m.pendingCaptures.Delete(id)
		m.gate.release(key)
		return Job{}, err
	}
```

After `cmd.Start()` fails (inside that error branch, before the return):

```go
		_ = m.store.Remove(id)
		m.pendingCaptures.Delete(id)
		m.gate.release(key)
		return Job{}, fmt.Errorf("spawn supervisor: %w", err)
```

In the UpdateMeta-failure goroutine:

```go
		go func() {
			_ = cmd.Wait()
			_ = m.store.Remove(id)
			m.pendingCaptures.Delete(id)
			m.gate.release(key)
		}()
```

(d) Replace the completion goroutine:

```go
	go func() {
		_ = cmd.Wait()
		// For a successful fresh run, capture the conversation id agy created
		// while the gate key is still held, then release. Gating on exit 0 avoids
		// waiting out the capture budget for a run that created no conversation.
		if code, ok := m.store.ExitCode(id); ok && code == 0 {
			m.captureFreshConversationID(&meta)
		}
		m.gate.release(key)
	}()
```

with:

```go
	go func() {
		_ = cmd.Wait()
		// For a successful fresh run, capture the conversation id agy created
		// while the gate key is still held, then release. Gating on exit 0 avoids
		// waiting out the capture budget for a run that created no conversation.
		if code, ok := m.store.ExitCode(id); ok && code == 0 {
			m.captureFreshConversationID(&meta)
		}
		// Settle the capture (success or give-up) before releasing the key, so
		// CapturePending=false means the reported status is final.
		m.pendingCaptures.Delete(id)
		m.gate.release(key)
	}()
```

(e) Add the accessor, after `lazyCaptureConversationID`:

```go
// CapturePending reports whether a fresh run's conversation-id capture has not
// yet settled in this process: capture was armed at start (or restore) and the
// post-exit capture attempt has not finished. Pollers use it to distinguish
// "done, id still being captured" from "done, no id is coming".
func (m *Manager) CapturePending(id string) bool {
	_, ok := m.pendingCaptures.Load(id)
	return ok
}
```

- [ ] **Step 4: Make agy_run_sync poll through a pending capture**

In `internal/mcptools/run_sync.go`, replace:

```go
			out := runSyncOutput{JobID: job.ID, statusOutput: toStatusOutput(st)}
			if st.State != manager.StateRunning {
				return nil, out, nil
			}
```

with:

```go
			out := runSyncOutput{JobID: job.ID, statusOutput: toStatusOutput(st)}
			if st.State != manager.StateRunning {
				// A fresh run's conversation id can lag the exit sentinel: agy's
				// cache daemon flushes after the process exits, and the manager's
				// completion goroutine captures the id with a bounded retry. While
				// that capture is still pending, keep polling instead of returning
				// done with no id: a sync caller has no reason to poll again after
				// a terminal result, so an id missing here is lost to the caller.
				if st.State == manager.StateDone && st.ConversationID == "" &&
					job.ConversationID == "" && time.Now().Before(deadline) &&
					mgr.CapturePending(job.ID) {
					select {
					case <-ctx.Done():
						return nil, out, nil
					case <-ticker.C:
					}
					continue
				}
				return nil, out, nil
			}
```

- [ ] **Step 5: Run the regression test**

Run: `go test ./internal/mcptools/ -run TestAgyRunSyncReturnsLateCapturedConversationID -v 2>&1 | tail -5`
Expected: PASS in roughly 1s (the 700ms cache lag plus a poll tick).

- [ ] **Step 6: Write the manager-level pending-lifecycle test**

Append to `internal/manager/capture_linux_test.go`:

```go
// CapturePending must be armed synchronously by a fresh StartJob and settle
// once the completion goroutine has captured (or given up on) the id.
func TestCapturePendingSettles(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	const newUUID = "55556666-7777-8888-9999-000011112222"

	c := config.Config{
		AgyPath: "/usr/bin/agy",
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
	if !m.CapturePending(job.ID) {
		t.Fatal("capture must be pending right after a fresh StartJob")
	}

	st := waitForCapturedID(t, m, job.ID, 3*time.Second)
	if st.ConversationID != newUUID {
		t.Fatalf("captured id = %q, want %q", st.ConversationID, newUUID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.CapturePending(job.ID) {
		if time.Now().After(deadline) {
			t.Fatal("capture never settled after the id was captured")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

- [ ] **Step 7: Run the affected packages, then the full suite**

Run: `go test ./internal/manager/ ./internal/mcptools/ 2>&1 | tail -5` then `go test ./... 2>&1 | tail -10`
Expected: all PASS. The existing run_sync tests now also flow through a real cache and settle quickly because the fake supervisor's cache write (no delay) lands before the completion goroutine's first capture attempt.

- [ ] **Step 8: Commit**

```bash
git add internal/manager/manager.go internal/manager/capture_linux_test.go internal/mcptools/run_sync.go internal/mcptools/run_sync_test.go
git commit -m "fix: agy_run_sync waits for a pending conversation-id capture

The manager tracks fresh runs whose capture has not settled (CapturePending);
agy_run_sync keeps polling a done job while its capture is pending instead of
returning done with an empty conversation_id, which lost the id for sync
callers whenever agy's cache flush lagged the exit sentinel."
```

---

### Task 5: Capture for restored jobs (watchRestored parity)

The StartJob completion goroutine captures while holding the gate key. The restored-job watcher releases the key with no capture attempt, so a fresh run that outlives a manager restart loses its id to the first same-cwd run that grabs the freed key. Mirror the completion path.

**Files:**
- Modify: `internal/manager/manager.go` (RestoreGate, watchRestored)
- Test: `internal/manager/restore_linux_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/manager/restore_linux_test.go`. Before writing, confirm `createLiveJob` (restore_linux_test.go:48) leaves `ConversationID` empty in the meta it writes (it must, since the restore tests exercise cwd-keyed conflicts); if it sets other fields like `CwdUUIDBefore`, leave them as it does.

```go
// A restored fresh run must have its conversation id captured by the watcher
// when its supervisor finishes, exactly like the StartJob completion path, and
// the id must land on disk without any Status call (the watcher, not a poller,
// owns the capture while the gate key is held).
func TestRestoreGateCapturesConversationIDOnExit(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := newManagerForRestore(t, exePath, 4)
	m.restoredPollInterval = 20 * time.Millisecond

	cwd := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.cacheFile = cachePath

	createLiveJob(t, m, "restored-fresh", cwd, pid)

	if err := m.RestoreGate(); err != nil {
		t.Fatalf("RestoreGate: %v", err)
	}
	if !m.CapturePending("restored-fresh") {
		t.Fatal("a restored fresh run must have its capture armed")
	}

	// The job "finishes": agy's cache gains the new conversation, then the
	// supervisor records exit 0 (the watcher treats the sentinel as terminal
	// even while the fake supervisor process lingers).
	const uuid = "deadbeef-1111-2222-3333-444455556666"
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, uuid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("restored-fresh", 0); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		meta, err := m.store.Load("restored-fresh")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if meta.ConversationID == uuid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher never captured the id; meta.ConversationID = %q", meta.ConversationID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

Add `"fmt"` and `"os"`/`"path/filepath"` to the file's imports if not already present.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/manager/ -run TestRestoreGateCapturesConversationIDOnExit -v 2>&1 | tail -5`
Expected: FAIL at `a restored fresh run must have its capture armed` (RestoreGate does not arm) or, if arming alone were added, at the capture wait.

- [ ] **Step 3: Arm in RestoreGate and capture in watchRestored**

In `internal/manager/manager.go`:

(a) In `RestoreGate`, replace:

```go
		key := keyFor(reqFromMeta(meta))
		if m.gate.forceAcquire(key) {
			m.watchRestored(meta, key)
		}
```

with:

```go
		key := keyFor(reqFromMeta(meta))
		if m.gate.forceAcquire(key) {
			if meta.ConversationID == "" && !meta.CaptureDisabled {
				// Mirror StartJob: arm the capture so pollers can tell this
				// restored fresh run's id is still being settled.
				m.pendingCaptures.Store(meta.ID, struct{}{})
			}
			m.watchRestored(meta, key)
		}
```

(b) In `watchRestored`, replace the goroutine body:

```go
	go func() {
		// Check once before waiting a full interval, so a supervisor that exits
		// right after startup does not hold its slot for a whole poll period.
		if !dead() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				if dead() {
					break
				}
			}
		}
		m.gate.release(key)
	}()
```

with:

```go
	go func() {
		// Check once before waiting a full interval, so a supervisor that exits
		// right after startup does not hold its slot for a whole poll period.
		if !dead() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				if dead() {
					break
				}
			}
		}
		// Mirror the StartJob completion path: a restored fresh run that exited 0
		// still needs its conversation id captured, and like there the capture
		// must happen while the gate key is held, so a new same-cwd run cannot
		// overwrite the cache entry first.
		if code, ok := m.store.ExitCode(meta.ID); ok && code == 0 {
			m.captureFreshConversationID(&meta)
		}
		m.pendingCaptures.Delete(meta.ID)
		m.gate.release(key)
	}()
```

- [ ] **Step 4: Run the test and the restore suite**

Run: `go test ./internal/manager/ -run 'TestRestoreGate' -v 2>&1 | tail -12`
Expected: all restore tests PASS, including the new one. The pre-existing release tests still pass: for their jobs the sentinel is absent or nonzero, so the new capture call is a no-op before release.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/manager.go internal/manager/restore_linux_test.go
git commit -m "fix: capture conversation ids for restored jobs before releasing the gate

watchRestored now mirrors the StartJob completion path: when a restored fresh
run's supervisor exits 0, the watcher captures the id while still holding the
cwd gate key, instead of releasing immediately and leaving the id to a lazy
single-shot Status capture that a new same-cwd run can spoil."
```

---

### Task 6: Lazy-capture guards: later-run check and settle memo

`lazyCaptureConversationID` (the post-restart fallback inside Status) holds no gate key, so a cache change since the snapshot may belong to a later same-cwd run; today it captures and persists it anyway, permanently mislabeling the job. Two guards: skip (permanently) when a later same-cwd job exists in the store, and settle permanently once the run is so long over that no attributable change can still appear. The settle memo also kills the per-poll cache re-read churn for id-less done jobs.

**Files:**
- Modify: `internal/manager/manager.go`
- Test: `internal/manager/capture_linux_test.go`

- [ ] **Step 1: Write the failing misattribution test**

Append to `internal/manager/capture_linux_test.go`:

```go
// A lazily captured id must not be stolen from a later same-cwd run: when
// another job started in this cwd after this one, a changed cache entry cannot
// be attributed to this job.
func TestStatusLazyCaptureSkipsWhenLaterRunExists(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	cwd := t.TempDir()
	const laterUUID = "99998888-7777-6666-5555-444433332222"
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, laterUUID)), 0o644); err != nil {
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

	// Job A: a completed fresh run from a previous manager, id never captured.
	metaA := jobstore.Meta{
		ID:            "job-a",
		Cwd:           cwd,
		CwdUUIDBefore: "",
		StartedAt:     time.Now().Add(-2 * time.Minute),
		Timeout:       time.Hour,
	}
	dirA, err := m.store.Create(metaA)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "out"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode(metaA.ID, 0); err != nil {
		t.Fatal(err)
	}
	// Job B: a later run in the same cwd; the cache entry is (or may be) B's.
	metaB := jobstore.Meta{ID: "job-b", Cwd: cwd, StartedAt: time.Now().Add(-time.Minute)}
	if _, err := m.store.Create(metaB); err != nil {
		t.Fatalf("Create: %v", err)
	}

	st, err := m.Status(metaA.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.ConversationID != "" {
		t.Fatalf("must not attribute the later run's id, got %q", st.ConversationID)
	}
	reloaded, err := m.store.Load(metaA.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.ConversationID != "" {
		t.Fatalf("misattributed id was persisted: %q", reloaded.ConversationID)
	}
}

// Once a job is long past its timeout with no cache change, lazy capture must
// settle permanently: a cache change appearing afterward belongs to some other
// (possibly out-of-store) run and must not be captured, and settled jobs must
// stop re-reading the cache on every poll.
func TestStatusLazyCaptureSettlesAfterHorizon(t *testing.T) {
	state := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	cwd := t.TempDir()
	if err := os.WriteFile(cachePath, []byte(`{}`), 0o644); err != nil {
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

	// Done long ago: StartedAt is far past Timeout + captureBudget.
	meta := jobstore.Meta{
		ID:            "job-old",
		Cwd:           cwd,
		CwdUUIDBefore: "",
		StartedAt:     time.Now().Add(-time.Hour),
		Timeout:       time.Minute,
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

	// First Status: no cache change, and the job is long over, so the capture settles.
	if st, err := m.Status(meta.ID); err != nil || st.ConversationID != "" {
		t.Fatalf("first Status: id=%q err=%v", st.ConversationID, err)
	}
	// A cache entry appears later (some unrelated run); it must not be captured.
	if err := os.WriteFile(cachePath, []byte(fmt.Sprintf(`{%q:%q}`, cwd, "abcdef00-1111-2222-3333-444455556666")), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := m.Status(meta.ID); err != nil || st.ConversationID != "" {
		t.Fatalf("settled job captured a late id: id=%q err=%v", st.ConversationID, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/manager/ -run 'TestStatusLazyCaptureSkipsWhenLaterRunExists|TestStatusLazyCaptureSettlesAfterHorizon' -v 2>&1 | tail -8`
Expected: both FAIL: the first captures `laterUUID`, the second captures the late id on the second Status.

- [ ] **Step 3: Implement the guards**

In `internal/manager/manager.go`:

(a) Add to the Manager struct, after `pendingCaptures`:

```go
	// settledCapture memoizes job ids whose lazy capture is permanently over
	// (no id is coming): either the run is long past its timeout with no cache
	// change, or a later same-cwd run made attribution unsafe. Settled jobs
	// stop re-reading the cache on every Status poll.
	settledMu      sync.Mutex
	settledCapture map[string]struct{}
```

(b) In `New`, add to the struct literal:

```go
		settledCapture:       make(map[string]struct{}),
```

(c) Add the helpers, after `CapturePending`:

```go
func (m *Manager) captureSettled(id string) bool {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	_, ok := m.settledCapture[id]
	return ok
}

func (m *Manager) settleCapture(id string) {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	m.settledCapture[id] = struct{}{}
}

// maybeSettleCapture marks a job's lazy capture as permanently over once the
// run is certainly long finished: the supervisor's hard timeout bounds the run
// and agy's cache daemon flushes within moments of the exit, so past
// StartedAt+Timeout+captureBudget no attributable cache change can still
// appear. Settling stops later polls from re-reading the cache for a job that
// will never get an id, and keeps a much-later unrelated cache write from
// being misattributed to this job.
func (m *Manager) maybeSettleCapture(meta jobstore.Meta) {
	horizon := meta.Timeout
	if horizon <= 0 {
		horizon = time.Hour // old metas without a recorded timeout: stay conservative
	}
	if time.Since(meta.StartedAt) > horizon+m.captureBudget {
		m.settleCapture(meta.ID)
	}
}

// hasLaterSameCwdRun reports whether any other stored job shares meta's cwd and
// started after it. When one exists, a changed cache entry cannot be attributed
// to meta's run: the later run may be the one that wrote it.
func (m *Manager) hasLaterSameCwdRun(meta jobstore.Meta) bool {
	ids, err := m.store.List()
	if err != nil {
		return true // cannot prove safety: skip the capture rather than risk misattribution
	}
	for _, id := range ids {
		if id == meta.ID {
			continue
		}
		other, err := m.store.Load(id)
		if err != nil {
			continue
		}
		if other.Cwd == meta.Cwd && other.StartedAt.After(meta.StartedAt) {
			return true
		}
	}
	return false
}
```

(d) Replace `lazyCaptureConversationID` (body and doc comment, which currently makes a false safety claim):

```go
// lazyCaptureConversationID best-effort captures a fresh run's conversation id
// from the cache when no in-process watcher captured it (the manager was
// restarted after the job ended). It returns an already-known id unchanged.
//
// No gate key is held here, so a cache change since the snapshot is not
// necessarily this job's: a later same-cwd run may have written it. Two guards
// keep that from becoming a persisted misattribution: a changed entry is not
// captured while any later same-cwd job exists in the store, and once the run
// is long enough over that no attributable change can still appear, the
// capture settles permanently as empty.
func (m *Manager) lazyCaptureConversationID(meta jobstore.Meta) string {
	if meta.ConversationID != "" {
		return meta.ConversationID
	}
	if meta.CaptureDisabled || m.captureSettled(meta.ID) {
		return ""
	}
	id, ok := captureNewUUID(m.cacheFile, meta.Cwd, meta.CwdUUIDBefore)
	if !ok {
		m.maybeSettleCapture(meta)
		return ""
	}
	if m.hasLaterSameCwdRun(meta) {
		m.settleCapture(meta.ID)
		return ""
	}
	final, err := m.store.SetConversationID(meta.ID, id)
	if err != nil {
		log.Printf("agy-mcp: persist captured conversation id for job %s: %v", meta.ID, err)
		return id // best-effort: report what we captured even if the persist failed
	}
	return final
}
```

- [ ] **Step 4: Run the lazy-capture tests, old and new**

Run: `go test ./internal/manager/ -run 'TestStatusLazy' -v 2>&1 | tail -10`
Expected: all four PASS (`LazilyCaptures`, `NoOpWhenCacheUnchanged`, `SkipsWhenLaterRunExists`, `SettlesAfterHorizon`). The first two stay green: their metas are 1 minute old with no recorded Timeout, so the 1-hour conservative horizon does not settle them, and no later same-cwd job exists.

- [ ] **Step 5: Full suite and commit**

Run: `go test ./... 2>&1 | tail -10`
Expected: all PASS.

```bash
git add internal/manager/manager.go internal/manager/capture_linux_test.go
git commit -m "fix: lazy conversation-id capture cannot steal a later run's id

Status's post-restart lazy capture now skips (and permanently settles) when a
later same-cwd job exists, and settles once the run is long past its timeout,
so a cache change from another run is never persisted as this job's id. The
settle memo also stops id-less done jobs from re-reading the cache per poll."
```

---

### Task 7: Documentation truth-up and final sweep

**Files:**
- Modify: `internal/manager/manager.go` (struct comment)
- Modify: `internal/manager/capture_linux_test.go` (gofmt drift at line 27)

- [ ] **Step 1: Fix the stale concurrency claim in the Manager struct comment**

In `internal/manager/manager.go`, in the comment block above `captureBudget`, replace the sentence:

```
// file lock), so a concurrent read can be torn; loadCache tolerates that (an
// unparsable read yields no capture) and this retry loop re-reads, so no mutex
// is needed.
```

with:

```
// file lock), so a concurrent read can be torn; loadCache reports torn reads
// as errors, capture treats them as "no capture yet" and this retry loop
// re-reads, and StartJob disables capture when the pre-run snapshot itself is
// unreadable, so no mutex is needed.
```

- [ ] **Step 2: Format, vet, lint**

Run: `gofmt -l . && go vet ./...`
Expected: no gofmt output (the pre-existing drift in `internal/manager/capture_linux_test.go:27` is fixed by `gofmt -w internal/manager/capture_linux_test.go` if it appears), no vet errors.

Run: `golangci-lint run 2>&1 | tail -5`
Expected: `0 issues`.

- [ ] **Step 3: Full suite, race detector**

Run: `go test -race ./... 2>&1 | tail -10`
Expected: all PASS. The race detector matters here: this change adds a sync.Map and a mutex-guarded map touched from the completion goroutine, the restored watcher, and Status callers.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "docs: align cache-concurrency comments with error-aware loadCache"
```

- [ ] **Step 5: Pre-push gate**

Per the repo workflow (recon, design, implement, gate, push), run the `gate` skill over the branch before pushing, then push and open a PR referencing issue #23 (`Fixes #23`).

---

## Self-Review Notes

- Issue #23 checkbox coverage: sync done-without-id (Task 4), fixture ordering (Task 1), lazy misattribution (Task 6), watchRestored (Task 5), loadCache errors + snapshot edge (Task 2), per-poll cache churn (Task 6 settle memo), wrong comments (Tasks 2, 6, 7).
- Type consistency: `snapshotCwd(cacheFile, cwd) (string, bool)` defined in Task 2 and consumed in Tasks 2 and 4's StartJob block; `pendingCaptures sync.Map` defined in Task 4, used in Task 5; `CapturePending` defined in Task 4, used in Tasks 4 and 5's tests; `ConversationCacheFile` defined in Task 3, used in Task 4's tests; `CacheDelay time.Duration` defined in Task 1, used in Task 4's regression test.
- Known interactions verified against current sources: `statusOutput` serializes the id as `conversation_id` (tools.go:57); `FakeAgy{Stdout, Stderr, Exit, SleepSecs}` (fakeagy.go:12); `SetConversationID` is first-write-wins under the store mutex, so concurrent lazy and goroutine captures stay benign.
- Deliberate non-goals (tracked in other issues): cwd normalization (#24), cross-process gate (#25), Status output polish (#29), test-suite hygiene (#34).
