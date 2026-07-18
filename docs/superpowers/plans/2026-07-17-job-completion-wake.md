# Job Completion Wake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let callers block on an agy job with one call (`agy_wait` tool, `wait-job` CLI) and let Claude Code be woken on job completion with zero polling (`hook-wait` CLI driven by an asyncRewake PostToolUse hook).

**Architecture:** One shared wait primitive, `manager.WaitTerminal`, carries the proven `agy_run_sync` polling semantics (250ms Status polls, terminal detection, conversation-id capture grace). `agy_run_sync` is refactored onto it, `agy_wait` reuses the same mcptools wait helper, and two new positional CLI subcommands (`wait-job`, `hook-wait`) call it through a new agy-free config resolver.

**Tech Stack:** Go 1.26, github.com/modelcontextprotocol/go-sdk/mcp, stdlib only otherwise.

**Spec:** `specs/2026-07-17-job-completion-wake-design.md` (local, gitignored; read it first).

## Global Constraints

- Em dashes and en dashes are banned in every file, comment, string, and commit message; use `-`, `,`, `:`, or `;`.
- `agy_run_sync` wire behavior (outputs, notes, wait cap, capture grace, client-cancel error) must not change; its existing tests pass unmodified.
- Tests that exec the bash fake agy need build tag `//go:build linux || darwin` (repo convention: `*_posix_test.go`).
- Verification for every task: `go build ./...`, `go test ./... -race` (from repo root), and the task's focused test command.
- Commit style: `<package>: <imperative summary>` (see `git log --oneline`).
- specs/ and docs/ are gitignored; never `git add -f` them.
- Comments explain constraints and reasoning, not narration; match existing density.

---

### Task 1: manager.WaitTerminal shared wait primitive

**Files:**
- Create: `internal/manager/wait.go`
- Create: `internal/manager/wait_posix_test.go`

**Interfaces:**
- Consumes: `(*Manager).Status`, `(*Manager).CapturePending`, `StateRunning`/`StateDone` (all existing, `internal/manager/status.go`, `manager.go`).
- Produces: `func (m *Manager) WaitTerminal(ctx context.Context, id string, deadline time.Time, onTick func(Status)) (Status, bool, error)` and const `waitPollInterval = 250 * time.Millisecond`. Tasks 2, 3, 5, 6 rely on exactly this signature.

- [ ] **Step 1: Write failing tests**

`internal/manager/wait_posix_test.go`. Look at `internal/manager/capture_posix_test.go` first and reuse its existing helpers for building a Manager around `testutil.WriteFakeAgy` / `testutil.WriteFakeSupervisor` (do not invent a parallel helper if one exists; the mcptools variant to mirror is `newTestManager` in `internal/mcptools/run_sync_posix_test.go:47`).

```go
//go:build linux || darwin

package manager_test // or package manager, matching the existing test files' choice

// Test list (each is a separate func; use the package's existing manager-builder helper):
//
// TestWaitTerminalReturnsDoneJob: start a job with a fast fake agy
// (Stdout "OK", Exit 0), poll Status until done (existing waitForDone-style
// loop), then call WaitTerminal with a 5s deadline: it must return
// (st.State == StateDone, terminal == true, err == nil) immediately.
//
// TestWaitTerminalBlocksUntilDone: fake agy with Sleep: 1 * time.Second,
// call WaitTerminal right after StartJob with a 15s deadline: returns
// done/terminal, and st.Result == "OK".
//
// TestWaitTerminalDeadlineOverrun: fake agy with Sleep: 2 * time.Second,
// WaitTerminal with deadline time.Now().Add(100 * time.Millisecond):
// returns (st.State == StateRunning, terminal == false, err == nil).
// Then wait for the job to finish (waitForDone-style) so cleanup is quiet.
//
// TestWaitTerminalContextCancel: fake agy with Sleep: 2 * time.Second,
// ctx with cancel; cancel after WaitTerminal is observably polling (cancel
// from a time.AfterFunc(300*time.Millisecond, cancel)): returns
// errors.Is(err, context.Canceled), terminal == false.
//
// TestWaitTerminalUnknownJob: WaitTerminal(ctx, "nonexistent-id", ...)
// returns a non-nil error (store load failure), terminal == false.
//
// TestWaitTerminalOnTick: fake agy with Sleep: 1 * time.Second; onTick
// increments an int (no mutex needed: onTick is documented single-goroutine).
// After WaitTerminal returns done, the count must be >= 1 and every observed
// Status passed to onTick had State == StateRunning.
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/manager/ -run TestWaitTerminal -v`
Expected: FAIL, `undefined: ... WaitTerminal` (compile error).

- [ ] **Step 3: Implement WaitTerminal**

`internal/manager/wait.go`:

```go
package manager

import (
	"context"
	"time"
)

// waitPollInterval is how often WaitTerminal re-reads job status and invokes
// onTick. Status reads a few small files, so this is cheap. agy_run_sync's
// wait loop used the same 250ms cadence before it moved here.
const waitPollInterval = 250 * time.Millisecond

// WaitTerminal polls the job until it reaches a terminal state, the deadline
// passes, or ctx is cancelled. It returns the latest observed Status and
// whether the job was terminal when the wait ended. The deadline is observed
// on poll ticks only, so a wait can overshoot it by up to one poll interval.
//
// onTick, if non-nil, is invoked once per poll from the calling goroutine
// while the job is still running; it must not block longer than the poll
// interval (it is for progress reporting, not work).
//
// One refinement carried over from the original agy_run_sync loop: when the
// job is done but its conversation id is still being captured in this process
// (CapturePending), WaitTerminal keeps polling until the capture settles or
// the deadline passes, so a caller that will never poll again does not lose
// the id to agy's cache-flush lag. If ctx is cancelled during that grace the
// job is already terminal, so the status is returned with a nil error.
// Cross-process callers always observe CapturePending == false; for them
// Status lazy-captures on read instead and no grace applies.
func (m *Manager) WaitTerminal(ctx context.Context, id string, deadline time.Time, onTick func(Status)) (Status, bool, error) {
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()
	for {
		st, err := m.Status(id)
		if err != nil {
			return Status{}, false, err
		}
		if st.State != StateRunning {
			if st.State == StateDone && st.ConversationID == "" &&
				time.Now().Before(deadline) && m.CapturePending(id) {
				select {
				case <-ctx.Done():
					return st, true, nil
				case <-ticker.C:
				}
				continue
			}
			return st, true, nil
		}
		if time.Now().After(deadline) {
			return st, false, nil
		}
		if onTick != nil {
			onTick(st)
		}
		select {
		case <-ctx.Done():
			return st, false, ctx.Err()
		case <-ticker.C:
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/manager/ -run TestWaitTerminal -race -v`
Expected: PASS (all six).

- [ ] **Step 5: Full package check and commit**

Run: `go build ./... && go test ./internal/manager/ -race`
Expected: PASS.

```bash
git add internal/manager/wait.go internal/manager/wait_posix_test.go
git commit -m "manager: add WaitTerminal shared wait primitive"
```

---

### Task 2: route agy_run_sync through a shared awaitJob helper

**Files:**
- Create: `internal/mcptools/await.go`
- Modify: `internal/mcptools/run_sync.go`

**Interfaces:**
- Consumes: `manager.WaitTerminal` (Task 1, exact signature above); existing `runSyncOutput`, `toStatusOutput`, `defaultSyncWait`, `maxSyncWait` in `internal/mcptools`.
- Produces: `func awaitJob(ctx context.Context, req *mcp.CallToolRequest, mgr *manager.Manager, jobID string, deadline time.Time) (runSyncOutput, error)` and `func parseWait(s string) (time.Duration, error)`. Task 3 relies on both.

- [ ] **Step 1: Write awaitJob and parseWait**

`internal/mcptools/await.go`:

```go
package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// notifyTimeout bounds each progress-notification send so a client that
// stopped draining the stream cannot park the wait loop past its cap. It
// matches the manager's poll cadence, which is what the send used to be
// bounded by when the loop lived in run_sync.
const notifyTimeout = 250 * time.Millisecond

// parseWait validates a caller-supplied inline wait, applying the shared
// default and cap. agy_run_sync and agy_wait use it so the two cannot drift.
func parseWait(s string) (time.Duration, error) {
	if s == "" {
		return defaultSyncWait, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid wait %q: want a positive Go duration like 90s", s)
	}
	return min(d, maxSyncWait), nil
}

// awaitJob blocks until the job is terminal or the deadline passes, emitting
// once-per-second MCP progress notifications when the client supplied a
// progress token. It is the wait phase shared by agy_run_sync and agy_wait,
// so their semantics cannot drift. On a wait-cap overrun the returned output
// carries the standard poll-with-agy_status note.
func awaitJob(ctx context.Context, req *mcp.CallToolRequest, mgr *manager.Manager, jobID string, deadline time.Time) (runSyncOutput, error) {
	token := req.Params.GetProgressToken()
	// Notify once per elapsed second, not per poll tick: the message has
	// whole-second granularity, so finer cadence is pure stream noise.
	lastNotified := time.Duration(-1)
	onTick := func(st manager.Status) {
		sec := st.Elapsed.Truncate(time.Second)
		if token == nil || sec == lastNotified {
			return
		}
		lastNotified = sec
		// Best effort: the result, not the notifications, is the contract.
		nctx, ncancel := context.WithTimeout(ctx, notifyTimeout)
		_ = req.Session.NotifyProgress(nctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      st.Elapsed.Seconds(),
			Message:       fmt.Sprintf("job %s running (%s)", jobID, sec),
		})
		ncancel()
	}
	st, terminal, err := mgr.WaitTerminal(ctx, jobID, deadline, onTick)
	if err != nil {
		if ctx.Err() != nil {
			// The client gave up on the call; the job stays alive under its
			// detached supervisor. Carry the job id in the error so a
			// gracefully-cancelling client can still find the job.
			return runSyncOutput{}, fmt.Errorf("wait cancelled; job %s is still running, poll it with agy_status: %w", jobID, err)
		}
		return runSyncOutput{}, fmt.Errorf("job %s status read failed: %w", jobID, err)
	}
	out := runSyncOutput{JobID: jobID, statusOutput: toStatusOutput(st)}
	if !terminal {
		out.Note = "wait cap reached; the job is still running, poll it with agy_status"
	}
	return out, nil
}
```

- [ ] **Step 2: Refactor registerRunSync onto it**

Replace the body of the `mcp.AddTool` handler in `internal/mcptools/run_sync.go` (keep the tool name and Description unchanged; delete the now-dead loop, the `syncPollInterval` constant, and unused imports):

```go
	}, func(ctx context.Context, req *mcp.CallToolRequest, in runSyncInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait, err := parseWait(in.Wait)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		startReq, err := in.toStartRequest()
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		job, err := mgr.StartJob(startReq)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		out, err := awaitJob(ctx, req, mgr, job.ID, time.Now().Add(wait))
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		return nil, out, nil
	})
```

Note the one accepted wire delta (spec-approved): the mid-wait status-read error text becomes `job <id> status read failed: ...` instead of `job <id> started but status read failed: ...`. No test asserts the old text; do not preserve it.

- [ ] **Step 3: Run the full existing run_sync suite**

Run: `go test ./internal/mcptools/ -race -v -run TestAgyRunSync`
Expected: PASS with zero test-file edits. If any of these tests needed changing, the refactor is wrong; fix the code, not the tests.

- [ ] **Step 4: Full build, vet, and commit**

Run: `go build ./... && go vet ./... && go test ./internal/mcptools/ ./internal/manager/ -race`
Expected: PASS.

```bash
git add internal/mcptools/await.go internal/mcptools/run_sync.go
git commit -m "mcptools: route agy_run_sync wait through manager.WaitTerminal"
```

---

### Task 3: agy_wait MCP tool

**Files:**
- Create: `internal/mcptools/wait.go`
- Create: `internal/mcptools/wait_posix_test.go`
- Modify: `internal/mcptools/tools.go` (const block at line 33)
- Modify: the server tool registration list (find it: `grep -n registerRunSync internal/mcptools/*.go`; add `registerWait(s, mgr)` beside it)

**Interfaces:**
- Consumes: `awaitJob`, `parseWait` (Task 2), `runSyncOutput`, `manager.Status`.
- Produces: MCP tool `agy_wait` with input `{ job_id, wait? }` and the `agy_run_sync` output shape `{ job_id, state, elapsed, result?, error?, conversation_id?, note? }`. Task 7 documents it.

- [ ] **Step 1: Write failing tests**

`internal/mcptools/wait_posix_test.go` (`//go:build linux || darwin`), reusing `connect`, `newTestManager`, `structMap`, `waitForDone`, `waitForRunningJob` from `run_sync_posix_test.go`:

```go
// TestAgyWaitReturnsResult: newTestManager with FakeAgy{Stdout: "WAITED OK",
// Exit: 0, Sleep: 500 * time.Millisecond}. CallTool agy_run (prompt "review"),
// take job_id from structMap. CallTool agy_wait {job_id, wait: "30s"}:
// state done, result "WAITED OK", job_id echoed back.
//
// TestAgyWaitUnknownJob: CallTool agy_wait {job_id: "no-such-job"}:
// err != nil || res.IsError must hold.
//
// TestAgyWaitInvalidWait: for each wait in "nope", "-1s", "0": CallTool
// agy_wait {job_id: "x", wait: wait} must error (wait is validated before
// the job id is looked up, mirroring run_sync's precedence).
//
// TestAgyWaitOverrunReturnsNote: FakeAgy{Stdout: "LATE OK", Exit: 0,
// Sleep: 2 * time.Second}; agy_run, then agy_wait {job_id, wait: "100ms"}:
// state running, note non-empty, and the job must NOT be cancelled:
// waitForDone(t, mgr, jobID, "LATE OK", 15*time.Second).
//
// TestAgyWaitSendsProgress: FakeAgy{Stdout: "OK", Exit: 0, Sleep: 1 * time.Second};
// agy_run, then agy_wait with params.SetProgressToken("tok-9") and wait "30s":
// completes done and at least one notification with token "tok-9" arrives
// (copy the deadline-drain pattern from TestAgyRunSyncSendsProgress).
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcptools/ -run TestAgyWait -v`
Expected: FAIL, tool `agy_wait` not found (CallTool error).

- [ ] **Step 3: Implement the tool**

Add to the const block in `internal/mcptools/tools.go`:

```go
	toolAgyWait      = "agy_wait"
```

`internal/mcptools/wait.go`:

```go
package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// waitInput is the input for agy_wait.
type waitInput struct {
	JobID string `json:"job_id" jsonschema:"the job id to wait for"`
	Wait  string `json:"wait,omitempty" jsonschema:"max time to wait inline (Go duration, default 2m, max 10m); on overrun the job keeps running and can be waited on again or polled with agy_status"`
}

// registerWait adds the agy_wait tool: block on an existing job until it
// finishes (bounded), streaming progress notifications when the client asked
// for them. It shares agy_run_sync's wait phase, so one agy_wait call
// replaces an agy_status poll loop.
func registerWait(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name: toolAgyWait,
		Description: "Block until an agy_run job finishes (bounded by wait, default 2m, max 10m). " +
			"Sends MCP progress notifications while waiting. Prefer one agy_wait over repeated " +
			"agy_status polling when the next step depends on the job's result. If the job " +
			"outlives the wait cap it keeps running; call agy_wait again or poll agy_status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, runSyncOutput, error) {
		wait, err := parseWait(in.Wait)
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		// Reject an unknown job before entering the wait loop, so a typo'd id
		// fails fast instead of polling a job that can never appear.
		if _, err := mgr.Status(in.JobID); err != nil {
			return nil, runSyncOutput{}, fmt.Errorf("job %s: %w", in.JobID, err)
		}
		out, err := awaitJob(ctx, req, mgr, in.JobID, time.Now().Add(wait))
		if err != nil {
			return nil, runSyncOutput{}, err
		}
		return nil, out, nil
	})
}
```

Register it beside the other tools (same registration site as `registerRunSync`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mcptools/ -run TestAgyWait -race -v`
Expected: PASS (all five).

- [ ] **Step 5: Full check and commit**

Run: `go build ./... && go test ./internal/mcptools/ -race`
Expected: PASS.

```bash
git add internal/mcptools/wait.go internal/mcptools/wait_posix_test.go internal/mcptools/tools.go internal/mcptools/serve.go
git commit -m "mcptools: add agy_wait tool for blocking on an existing job"
```

(If registration lives somewhere other than serve.go, add that file instead.)

---

### Task 4: config.ResolveWait, an agy-free resolver

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `func ResolveWait() (Config, error)` returning a Config with StateDir plus defaults (`DefaultTimeout: 30 * time.Minute`, `MaxConcurrency: 4`, `JobTTL: 24 * time.Hour`) and empty AgyPath/SupervisorExe. Tasks 5 and 6 call it.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
// TestResolveWaitNeedsNoAgy: ResolveWait must succeed with no agy anywhere on
// PATH (the wait-only subcommands are pure observers of the job store), while
// Resolve fails in the same environment, proving the seam matters.
func TestResolveWaitNeedsNoAgy(t *testing.T) {
	t.Setenv("AGY_MCP_AGY_PATH", "")
	t.Setenv("PATH", t.TempDir()) // empty dir: no agy
	t.Setenv("AGY_MCP_STATE_DIR", "/custom/state")

	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve succeeded without agy on PATH; the control condition is broken")
	}
	c, err := ResolveWait()
	if err != nil {
		t.Fatalf("ResolveWait: %v", err)
	}
	if c.StateDir != "/custom/state" {
		t.Fatalf("StateDir = %q, want /custom/state", c.StateDir)
	}
	if c.AgyPath != "" || c.SupervisorExe != "" {
		t.Fatalf("wait config resolved binaries it must not need: agy=%q supervisor=%q", c.AgyPath, c.SupervisorExe)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestResolveWaitNeedsNoAgy -v`
Expected: FAIL, `undefined: ResolveWait`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, extract the state-dir block of `Resolve` (the `stateRoot := os.Getenv("AGY_MCP_STATE_DIR")` paragraph) into a helper, call it from `Resolve`, and add `ResolveWait`:

```go
// resolveStateDir returns the job-state root: AGY_MCP_STATE_DIR verbatim, or
// the XDG state-home fallback. Shared by Resolve and ResolveWait so the two
// cannot drift.
func resolveStateDir() (string, error) {
	if stateRoot := os.Getenv("AGY_MCP_STATE_DIR"); stateRoot != "" {
		return stateRoot, nil
	}
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		xdg = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(xdg, "agy-mcp"), nil
}

// ResolveWait builds the minimal Config the wait-only subcommands (wait-job,
// hook-wait) need: the state dir and defaults, with no agy binary lookup and
// no supervisor path. Reading job status never execs agy, so requiring it on
// PATH would be an artificial failure for a pure observer.
func ResolveWait() (Config, error) {
	stateDir, err := resolveStateDir()
	if err != nil {
		return Config{}, err
	}
	return Config{
		StateDir:       stateDir,
		DefaultTimeout: 30 * time.Minute,
		MaxConcurrency: 4,
		JobTTL:         24 * time.Hour,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -race -v`
Expected: PASS, including all pre-existing Resolve tests (the extraction must not change Resolve's behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: add ResolveWait for wait-only subcommands"
```

---

### Task 5: wait-job CLI subcommand

**Files:**
- Create: `waitcmd.go` (package main)
- Create: `waitcmd_posix_test.go` (package main)
- Modify: `main.go:33-43` (subcommand dispatch)

**Interfaces:**
- Consumes: `config.ResolveWait` (Task 4), `manager.New`, `manager.WaitTerminal` (Task 1), `manager.Status`.
- Produces: `func waitJobMain(args []string, stdout, stderr io.Writer) int` and `func waitForJob(id string, timeout time.Duration) (manager.Status, bool, error)`; Task 6 reuses `waitForJob`. CLI contract (documented in Task 7): exit 0 terminal (state word on stdout: `done`, `failed`, or `cancelled`), 1 error, 2 usage, 3 timeout.

- [ ] **Step 1: Write failing tests**

`waitcmd_posix_test.go` (`//go:build linux || darwin`, package main). Two fixtures:

```go
// writeTerminalJob creates <stateDir>/jobs/<id> holding a done job: meta.json
// (jobstore.Meta{ID: id, StartedAt: time.Now(), CaptureDisabled: true}),
// out file "RESULT", exit_code "0". CaptureDisabled stops the wait manager
// from reading the host's real agy conversation cache during the test.
// Write files with jsonMarshalForTest + jobstore.MetaPath / jobstore.OutPath /
// jobstore.ExitCodePath (see TestRunJobSubcommandEndToEnd in main_test.go for
// the meta-writing pattern). Also t.Setenv("HOME", t.TempDir()) in every test
// so the manager's default cache path can never touch the real user cache.
//
// TestWaitJobDoneJob: fixture job "job-done-1"; t.Setenv AGY_MCP_STATE_DIR to
// the fixture stateDir; waitJobMain([]string{"job-done-1"}, &out, &errb)
// returns 0 and out.String() == "done\n".
//
// TestWaitJobUnknownJob: same env, waitJobMain([]string{"nope"}, ...) returns 1
// and stderr mentions the failure.
//
// TestWaitJobUsage: waitJobMain(nil, ...) returns 2. waitJobMain
// ([]string{"a", "b"}, ...) returns 2.
//
// TestWaitJobTimeout: start a real slow job: build a manager the way
// internal/mcptools' newTestManager does (testutil.WriteFakeAgy with
// Sleep: 2 * time.Second, testutil.WriteFakeSupervisor, config.Config with
// ConversationCacheFile pointing at a temp cache) over a temp stateDir;
// StartJob; t.Setenv AGY_MCP_STATE_DIR to that stateDir; waitJobMain
// ([]string{"-timeout", "100ms", job.ID}, ...) returns 3. Then poll the
// starting manager until the job is done so nothing leaks past the test.
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run TestWaitJob -v`
Expected: FAIL, `undefined: waitJobMain`.

- [ ] **Step 3: Implement**

`waitcmd.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// waitJobMain implements "agy-mcp wait-job [-timeout 1h] <job_id>": block
// until the job reaches a terminal state, print that state word to stdout,
// and exit 0. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout. It is the
// scriptable face of manager.WaitTerminal for shell automation that would
// otherwise poll agy_status.
func waitJobMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait-job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: agy-mcp wait-job [-timeout 1h] <job_id>")
		return 2
	}
	id := fs.Arg(0)
	st, terminal, err := waitForJob(id, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "wait-job: %v\n", err)
		return 1
	}
	if !terminal {
		fmt.Fprintf(stderr, "wait-job: job %s still running after %s\n", id, *timeout)
		return 3
	}
	fmt.Fprintln(stdout, st.State)
	return 0
}

// waitForJob resolves the wait-only config and blocks on the job. Shared by
// wait-job and hook-wait so the two subcommands cannot diverge on how a wait
// manager is built. The job itself is never signalled: SIGINT/SIGTERM cancel
// only this observer's wait.
func waitForJob(id string, timeout time.Duration) (manager.Status, bool, error) {
	cfg, err := config.ResolveWait()
	if err != nil {
		return manager.Status{}, false, err
	}
	mgr := manager.New(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return mgr.WaitTerminal(ctx, id, time.Now().Add(timeout), nil)
}
```

In `main.go`, after the existing `run-job` block (keep its shape):

```go
	if len(os.Args) >= 2 && os.Args[1] == "wait-job" {
		os.Exit(waitJobMain(os.Args[2:], os.Stdout, os.Stderr))
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestWaitJob -race -v`
Expected: PASS (all four).

- [ ] **Step 5: Full check and commit**

Run: `go build ./... && go test . -race`
Expected: PASS (including the pre-existing main tests).

```bash
git add waitcmd.go waitcmd_posix_test.go main.go
git commit -m "main: add wait-job subcommand"
```

---

### Task 6: hook-wait CLI subcommand and hookinput parser

**Files:**
- Create: `internal/hookinput/hookinput.go`
- Create: `internal/hookinput/hookinput_test.go`
- Create: `hookwait.go` (package main)
- Create: `hookwait_posix_test.go` (package main)
- Modify: `main.go` (dispatch, beside Task 5's block)

**Interfaces:**
- Consumes: `waitForJob` (Task 5), `config.ResolveWait` (Task 4), `manager.New`, `manager.Status`, `manager.StateRunning`.
- Produces: `func hookinput.Parse(r io.Reader) (jobID, toolName string, ok bool)`; `func hookWaitMain(args []string, stdin io.Reader, stderr io.Writer) int`. CLI contract: exit 2 wakes Claude (terminal or timeout, message on stderr); every other outcome exits 0 silently.

- [ ] **Step 1: Write failing parser tests**

`internal/hookinput/hookinput_test.go`, table-driven over realistic Claude Code PostToolUse payloads:

```go
package hookinput

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, in           string
		wantID, wantTool   string
		wantOK             bool
	}{
		{
			name: "structured job_id",
			in: `{"hook_event_name":"PostToolUse","tool_name":"mcp__agy__agy_run",
			      "tool_input":{"prompt":"review"},
			      "tool_response":{"job_id":"job-abc-123","state":"running"}}`,
			wantID: "job-abc-123", wantTool: "mcp__agy__agy_run", wantOK: true,
		},
		{
			name: "job_id nested in MCP content list",
			in: `{"tool_name":"mcp__agy__agy_run",
			      "tool_response":{"content":[{"type":"text","text":"{\"job_id\":\"job-xyz-9\",\"state\":\"running\"}"}]}}`,
			wantID: "job-xyz-9", wantTool: "mcp__agy__agy_run", wantOK: true,
		},
		{
			name:   "no job id",
			in:     `{"tool_name":"mcp__agy__agy_run","tool_response":{"error":"conflict"}}`,
			wantOK: false,
		},
		{
			name:   "malformed json",
			in:     `{"tool_name": `,
			wantOK: false,
		},
		{
			name:   "empty input",
			in:     ``,
			wantOK: false,
		},
		{
			name: "run_sync tool name carried through",
			in: `{"tool_name":"mcp__agy__agy_run_sync",
			      "tool_response":{"job_id":"job-s-1","state":"done"}}`,
			wantID: "job-s-1", wantTool: "mcp__agy__agy_run_sync", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, tool, ok := Parse(strings.NewReader(tc.in))
			if ok != tc.wantOK || id != tc.wantID || (ok && tool != tc.wantTool) {
				t.Fatalf("Parse = (%q, %q, %v), want (%q, %q, %v)", id, tool, ok, tc.wantID, tc.wantTool, tc.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/hookinput/ -v`
Expected: FAIL, package does not exist yet.

- [ ] **Step 3: Implement the parser**

`internal/hookinput/hookinput.go`:

```go
// Package hookinput extracts the agy job id from a Claude Code PostToolUse
// hook payload, for the hook-wait subcommand. Parsing is deliberately loose:
// the payload's tool_response shape depends on the MCP client version (plain
// structured output, or a content list whose text embeds the output JSON), so
// the job id is found by walking, not by a fixed schema.
package hookinput

import (
	"encoding/json"
	"io"
	"strings"
)

// maxDepth bounds the recursive walk so a pathological payload cannot
// exhaust the stack. Real payloads are a handful of levels deep.
const maxDepth = 32

// payload is the subset of the PostToolUse hook input hook-wait needs.
type payload struct {
	ToolName     string          `json:"tool_name"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// Parse decodes a PostToolUse payload from r and extracts the agy job id from
// its tool response. ok is false when no job id is present; that is not an
// error, a failed or foreign tool call simply has no job to wait for.
func Parse(r io.Reader) (jobID, toolName string, ok bool) {
	var p payload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return "", "", false
	}
	var resp any
	if err := json.Unmarshal(p.ToolResponse, &resp); err != nil {
		return "", p.ToolName, false
	}
	id := findJobID(resp, 0)
	return id, p.ToolName, id != ""
}

// findJobID walks maps and slices for a "job_id" string field. String values
// that look like they embed JSON with a job id (MCP text content) are parsed
// and walked too, so the id is found regardless of which layer carries it.
func findJobID(v any, depth int) string {
	if depth > maxDepth {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if id, isStr := t["job_id"].(string); isStr && id != "" {
			return id
		}
		for _, child := range t {
			if id := findJobID(child, depth+1); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range t {
			if id := findJobID(child, depth+1); id != "" {
				return id
			}
		}
	case string:
		if strings.Contains(t, `"job_id"`) {
			var embedded any
			if err := json.Unmarshal([]byte(t), &embedded); err == nil {
				return findJobID(embedded, depth+1)
			}
		}
	}
	return ""
}
```

- [ ] **Step 4: Parser tests pass, then write failing subcommand tests**

Run: `go test ./internal/hookinput/ -race -v` -> PASS.

`hookwait_posix_test.go` (`//go:build linux || darwin`, package main), reusing Task 5's `writeTerminalJob` fixture and env pattern (AGY_MCP_STATE_DIR + HOME both pointed at test dirs):

```go
// hookPayload builds the stdin JSON: fmt.Sprintf(`{"tool_name":%q,
// "tool_response":{"job_id":%q,"state":"running"}}`, tool, id).
//
// TestHookWaitWakesOnDoneJob: fixture "job-hw-1" (done). hookWaitMain(nil,
// strings.NewReader(hookPayload("mcp__agy__agy_run", "job-hw-1")), &errb)
// returns 2; stderr contains "job-hw-1" and "state=done" and "agy_status".
//
// TestHookWaitQuietWhenRunSyncAlreadyTerminal: same fixture, tool name
// "mcp__agy__agy_run_sync": returns 0, stderr empty (the sync caller already
// received the result inline; a wake would be noise).
//
// TestHookWaitQuietOnNoJobID: hookWaitMain(nil, strings.NewReader(
// `{"tool_name":"mcp__agy__agy_run","tool_response":{"error":"x"}}`), &errb)
// returns 0, stderr empty.
//
// TestHookWaitWakesOnTimeout: slow real job as in TestWaitJobTimeout
// (Sleep 2s fake, StartJob, env pointed at its stateDir); hookWaitMain(
// []string{"-timeout", "100ms"}, strings.NewReader(hookPayload(
// "mcp__agy__agy_run", job.ID)), &errb) returns 2 and stderr contains
// "still running". Drain the job afterwards.
//
// TestHookWaitQuietOnUnknownJob: env points at an empty state dir;
// payload references "job-none": returns 0 (the wait errors internally,
// and internal errors never wake or disrupt the session).
```

Run: `go test . -run TestHookWait -v` -> FAIL, `undefined: hookWaitMain`.

- [ ] **Step 5: Implement the subcommand**

`hookwait.go`:

```go
package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/hookinput"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// hookWaitMain implements "agy-mcp hook-wait [-timeout 1h]": read a Claude
// Code PostToolUse hook payload from stdin, wait for the agy job it
// references, and exit 2 with a wake message on stderr. Exit 2 is Claude
// Code's asyncRewake wake signal; every other outcome exits 0 with no output,
// because a hook that fails must never disrupt the tool flow it observes.
// A timeout also wakes (exit 2): the model should learn the job is
// long-running rather than never hearing back.
func hookWaitMain(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook-wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage noise would land in the transcript; stay quiet
	timeout := fs.Duration("timeout", time.Hour, "max time to wait for the job")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	jobID, toolName, ok := hookinput.Parse(stdin)
	if !ok {
		return 0
	}
	// A sync tool that already returned a terminal result delivered it inline;
	// waking Claude again would be noise. agy_run never delivers inline, so
	// for it even an already-terminal job still gets its wake.
	if strings.HasSuffix(toolName, "agy_run_sync") {
		if cfg, err := config.ResolveWait(); err == nil {
			if st, err := manager.New(cfg).Status(jobID); err == nil && st.State != manager.StateRunning {
				return 0
			}
		}
	}
	st, terminal, err := waitForJob(jobID, *timeout)
	if err != nil {
		return 0
	}
	if terminal {
		fmt.Fprintf(stderr, "agy job %s finished: state=%s elapsed=%s; call agy_status with this job_id to collect the result\n",
			jobID, st.State, st.Elapsed.Round(time.Second))
	} else {
		fmt.Fprintf(stderr, "agy job %s still running after %s; poll agy_status\n", jobID, *timeout)
	}
	return 2
}
```

In `main.go`, beside the Task 5 block:

```go
	if len(os.Args) >= 2 && os.Args[1] == "hook-wait" {
		os.Exit(hookWaitMain(os.Args[2:], os.Stdin, os.Stderr))
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test . ./internal/hookinput/ -race -v -run 'TestHookWait|TestParse'`
Expected: PASS.

- [ ] **Step 7: Full check and commit**

Run: `go build ./... && go test ./... -race`
Expected: PASS everywhere.

```bash
git add internal/hookinput/ hookwait.go hookwait_posix_test.go main.go
git commit -m "main: add hook-wait subcommand for Claude Code asyncRewake"
```

---

### Task 7: README documentation

**Files:**
- Modify: `README.md` (Tools list at line 75-82; new section after the Tools section, before "HTTP mode")

**Interfaces:**
- Consumes: final tool/subcommand names and semantics from Tasks 3, 5, 6 exactly as specified there.
- Produces: user-facing docs; nothing downstream.

- [ ] **Step 1: Add agy_wait to the Tools list**

After the `agy_status` line in the Tools list:

```markdown
- `agy_wait(job_id, wait?)` -> `{ job_id, state, elapsed, result?, error?, conversation_id?, note? }`
```

And in the "What it provides" bullet list near the top, extend the first bullet: `agy_run` / `agy_status` / `agy_cancel` stays, and after the `agy_run_sync` bullet add:

```markdown
- `agy_wait`: block until an already-started job finishes (bounded, with MCP progress notifications); one call replaces an `agy_status` poll loop.
```

- [ ] **Step 2: Add the completion-wake section**

New section after the Tools section (heading level matches "HTTP mode"). Prose paragraphs must each be one continuous line (no hard wrapping inside paragraphs; repo README follows this for new content, and GFM rendering is the reason). No em dashes. Content to cover, in this order:

```markdown
## Completion wake for Claude Code

Claude Code does not surface MCP server notifications to the model: progress notifications are UI-only, and there is no server-initiated push that can wake the model when an async job finishes. Polling `agy_status` after `agy_run` is the protocol-level baseline. agy-mcp ships a hook bridge that turns job completion into a real wake instead, built on Claude Code's `asyncRewake` hook mechanism: a background PostToolUse hook that exits with code 2 wakes the model with the hook's stderr as a system reminder.

Add to your Claude Code `settings.json`:

    {
      "hooks": {
        "PostToolUse": [
          {
            "matcher": "mcp__agy__agy_run(_sync)?",
            "hooks": [
              { "type": "command", "command": "agy-mcp hook-wait", "asyncRewake": true, "timeout": 3600 }
            ]
          }
        ]
      }
    }

How it behaves:

- After every `agy_run`, the hook waits (in the background, off the session's critical path) for the job's completion sentinel and then wakes Claude with a one-line message naming the job id and final state, so the model calls `agy_status` exactly once, when the result is actually ready.
- `agy_run_sync` calls that returned their result inline produce no wake; a sync call that overran its wait cap (and so returned a still-running job) gets the same completion wake as `agy_run`.
- On timeout (default 1h, `-timeout` to change) the hook still wakes Claude, reporting the job as still running, so a long job is never silently lost. On any internal error the hook exits 0 silently; it can never disrupt the tool call it observes.
- The matcher entry name `agy` is the server name from `claude mcp add agy`; adjust both if you registered the server under a different name.

Two related subcommands, useful beyond Claude Code:

- `agy-mcp wait-job [-timeout 1h] <job_id>` blocks until the job is terminal and prints the final state word (`done`, `failed`, or `cancelled`) to stdout. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout. It needs only the job state directory, not the agy binary, so it works in minimal environments.
- `agy-mcp hook-wait [-timeout 1h]` is the hook entrypoint described above: it reads the PostToolUse payload from stdin, so it is not useful to invoke by hand, but it is a single self-contained binary call, no shell wrapper or jq required, and it works on Linux, macOS, and Windows.

MCP clients other than Claude Code get the same no-polling benefit in-protocol: call `agy_wait` with the `job_id` returned by `agy_run` and the tool blocks (bounded by `wait`, default 2m, max 10m) until the job finishes.
```

- [ ] **Step 3: Verify rendering and style**

Run: `grep -nE '\x{2014}|\x{2013}' README.md || echo clean` (expect `clean`; also run the same grep over every file the branch touches).
Read the section back for hard-wrapped paragraphs (each prose paragraph must be a single line).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "README: document completion wake, agy_wait, and wait subcommands"
```

---

## Final verification (after all tasks)

- `go build ./...`
- `go test ./... -race`
- `golangci-lint run`
- `git log --oneline main..HEAD` shows one commit per task, package-prefixed.
