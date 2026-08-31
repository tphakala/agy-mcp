// Package manager owns agy job lifecycle: spawning, status, cancel, and the
// model/session queries. It is transport-agnostic.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// Manager coordinates jobs backed by an on-disk store.
type Manager struct {
	cfg       config.Config
	store     jobStore   // *jobstore.Store in production; an interface so tests can inject failures
	gate      *gate      // serializes conflicting jobs and caps total concurrency within this process
	xlock     *crossLock // serializes same-key jobs across sibling processes sharing the state dir
	cacheFile string     // agy conversation cache (last_conversations.json); injectable for tests

	// verifiedAgy is the agy path whose version has been checked and accepted in
	// this process; verifyMu guards it. Caching the path (rather than a bare
	// bool) means a configuration that resolves to a different binary is
	// re-checked instead of inheriting the previous verdict.
	verifyMu    sync.Mutex
	verifiedAgy string

	// readAgyVersion is an indirection over the free function of the same name so
	// a test can drive the version gate without a real agy on disk. Defaulted in
	// New; a field rather than a package var so tests stay isolated and parallel.
	readAgyVersion func(context.Context, string) (string, error)

	// Timing fields (not package globals) so tests stay isolated and can run in
	// parallel. conversationIDWait bounds AwaitConversationID's wait for agy's
	// init event; restoredPollInterval paces the liveness watcher for jobs whose
	// supervisor outlived a manager restart.
	conversationIDWait   time.Duration
	restoredPollInterval time.Duration

	// readStartTimeTicks is a per-instance indirection over the free function of
	// the same name (which has no fault-injection seam of its own), defaulted in
	// New, so a darwin-tagged test can force the startTimeMandatory fail-closed
	// spawn path in StartJob on its own *Manager without depending on an actual
	// sysctl read failure (not reliably reproducible for a real child process)
	// and without any risk of a package-level var leaking into another test.
	readStartTimeTicks func(int) (uint64, bool)

	// testHookMidRelease, when non-nil, is invoked by releaseKey between its two
	// unlock steps (the cross-process xlock.unlock and the in-process gate.release).
	// It exists only so a test can deterministically interpose a concurrent admit at
	// that intermediate point and pin the release-ordering invariant (issue #81);
	// production leaves it nil, so the check is a single never-taken branch.
	testHookMidRelease func()
}

// New constructs a Manager.
func New(c config.Config) *Manager {
	// Prefer an explicitly configured cache path; only fall back to the default
	// under HOME when none was given. Resolving the fallback here, rather than with
	// cmp.Or (which evaluates both args), keeps agyCachePath's home-dir lookup and
	// its failure log from running when an explicit ConversationCacheFile is set, so
	// that explicit path is never shadowed by a misleading "disabled" log.
	cacheFile := c.ConversationCacheFile
	if cacheFile == "" {
		cacheFile = agyCachePath()
	}
	return &Manager{
		cfg:                  c,
		store:                jobstore.New(c.StateDir),
		gate:                 newGate(c.MaxConcurrency),
		xlock:                newCrossLock(c.StateDir),
		cacheFile:            cacheFile,
		conversationIDWait:   c.ConversationIDWait,
		restoredPollInterval: 2 * time.Second,
		readStartTimeTicks:   readStartTimeTicks,
		readAgyVersion:       readAgyVersion,
	}
}

// StartRequest describes a run to start.
type StartRequest struct {
	Prompt         string
	Model          string   // optional; falls back to cfg.DefaultModel, then reduced by modelID
	Effort         string   // optional; --effort <low|medium|high>, reasoning effort for the session
	Mode           string   // optional; --mode <accept-edits|plan>, agy's agent execution mode
	Agent          string   // optional; --agent <name>, selects a specific agy agent
	Sandbox        bool     // optional; --sandbox, runs agy with terminal restrictions
	Dirs           []string // repeated --add-dir
	ConversationID string   // optional; --conversation <id>
	JSONSchema     string   // optional; --json-schema <inline schema or path>, constrains the final stream-json result
	ContinueLatest bool     // resolve cwd's latest conversation before the run
	Cwd            string   // optional; defaults to process cwd
	Timeout        time.Duration
}

// Job is the handle returned to callers.
type Job struct {
	ID             string
	ConversationID string
	State          string // "running"
}

// versionCheckTimeout bounds the `agy --version` probe so a wedged binary
// cannot park the tool call that triggered it.
const versionCheckTimeout = 10 * time.Second

// versionKillGrace is how long after the deadline kill the probe waits before
// abandoning its output pipes, so a descendant that inherited them cannot hold
// the probe open past its timeout.
const versionKillGrace = time.Second

// agyBinaryChecked resolves the agy binary and verifies it is new enough to
// drive, caching the verdict for the process.
//
// The check is deferred to first exec rather than done in config.Resolve for
// the same reason the PATH lookup is: initialize, tools/list and list_sessions
// never exec agy, so a server that refused to start without a suitable binary
// would deny a client the whole introspection surface over a binary it may not
// be about to use.
//
// Only success is cached. A missing or too-old agy re-checks on the next call,
// so installing or upgrading agy is picked up without restarting the server,
// which is the behaviour the deferred lookup already promises. The cost of that
// is one extra process spawn per call while agy is unusable, which is the error
// path.
func (m *Manager) agyBinaryChecked(ctx context.Context) (string, error) {
	agy, err := m.cfg.AgyBinary()
	if err != nil {
		return "", err
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	if m.verifiedAgy == agy {
		return agy, nil
	}
	raw, err := m.readAgyVersion(ctx, agy)
	if err != nil {
		return "", fmt.Errorf("determine version of agy at %s: %w", agy, err)
	}
	v, perr := agyver.Parse(raw)
	if perr != nil {
		return "", fmt.Errorf("parse agy version from %q (%s): %w", strings.TrimSpace(raw), agy, perr)
	}
	if !v.AtLeast(agyver.Required) {
		return "", fmt.Errorf(
			"agy %s or newer is required (agy-mcp drives agy through --output-format stream-json); found %s at %s",
			agyver.Required, v, agy)
	}
	m.verifiedAgy = agy
	return agy, nil
}

// readAgyVersion runs `agy --version` and returns its raw output.
//
// A non-zero exit is deliberately not treated as failure: --version is not
// listed in agy's --help, so its exit status is not a contract, and the output
// is what matters. Only a failure to execute at all (a missing or
// non-executable binary) is an error. stderr is folded in for the same reason,
// in case a future agy prints the version there.
func readAgyVersion(ctx context.Context, agy string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, agy, "--version")
	// Suppress the console window this probe would otherwise flash on Windows when
	// the manager itself has no console to inherit (an MCP client launching it from
	// a GUI app). No-op elsewhere.
	proc.ConfigureNoWindow(cmd)
	// Without this, killing the process on deadline does not unblock the output
	// copy: CombinedOutput reads through a pipe whose write end agy's descendants
	// inherit, so a grandchild that outlives the kill holds the read open and the
	// probe never returns. WaitDelay closes the pipes shortly after the kill.
	cmd.WaitDelay = versionKillGrace
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Check the deadline BEFORE classifying the exec error. A ctx-killed
		// process surfaces as *exec.ExitError, which is indistinguishable from a
		// binary that merely exited non-zero, so swallowing it would let whatever
		// a wedged agy happened to print be parsed as its version and cached as
		// verified for the life of the process.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				// The caller gave up. Say so, rather than blaming a binary that was
				// answering perfectly well.
				return "", fmt.Errorf("agy --version was cancelled: %w", ctxErr)
			}
			return "", fmt.Errorf("agy --version did not complete within %s: %w", versionCheckTimeout, ctxErr)
		}
		// WaitDelay fired while the deadline had NOT: agy answered and exited, but
		// a descendant kept the output pipe open, so exec abandoned the copy. The
		// version it printed is already buffered in out, and it is the whole point
		// of the probe. Refusing here would reject a working agy for the exact
		// reason WaitDelay was set in the first place. Fall through and let Parse
		// decide; if the output really was truncated, the parse failure says so.
		if !errors.Is(err, exec.ErrWaitDelay) {
			// A non-zero exit is not itself a failure: --version is not listed in
			// agy's --help, so its exit status is not a contract and the output is
			// what matters. Only a failure to execute at all is an error.
			if _, isExit := errors.AsType[*exec.ExitError](err); !isExit {
				return "", err
			}
		}
	}
	return string(out), nil
}

// conversationIDPoll is how often the conversation-id wait re-reads the
// progress file. Its budget is config.ConversationIDWait.
const conversationIDPoll = 50 * time.Millisecond

// AwaitConversationID reports the conversation a run belongs to. A run that
// already names one (a continuation) returns it at once; a fresh one polls the
// job's progress file until the supervisor records the id agy reported, the job
// turns out to be over, the caller cancels ctx, or the budget expires. It returns
// "" when no id arrived, which is not an error: the id is still delivered by the
// next Status read.
//
// It is deliberately not part of StartJob. Only agy_run needs the id in its own
// response, because it returns before the run produces anything else; agy_run_sync
// takes it off the Status it already polls. While StartJob did the wait, agy_run_sync
// paid it before its own inline-wait deadline was set (run_sync.go computes that
// deadline from the time StartJob returns), so a call that outran the deadline overran
// its documented wait cap by roughly the budget. A call whose run ends inside the wait
// saw no saving at all, because the wait overlapped the run rather than preceding it.
//
// The budget is why agy_run is not return-immediately (the agy version gate is the
// other bounded wait, and only until it succeeds once), so it exits at
// the first opportunity rather than always sleeping it out. A terminal job is
// one such opportunity: the supervisor writes the progress file while decoding
// the stream and the exit-code sentinel only at the very end, so a visible
// sentinel with no recorded id means none is coming (the run died before agy's
// init event, or never started at all).
func (m *Manager) AwaitConversationID(ctx context.Context, job Job) string {
	if job.ConversationID != "" {
		return job.ConversationID
	}
	// A job id this manager did not issue has no conversation to wait for, which is
	// the same answer an expired budget gives.
	dir, err := m.store.Dir(job.ID)
	if err != nil {
		return ""
	}
	deadline := time.Now().Add(m.conversationIDWait)
	for {
		if p, ok := jobstore.ReadProgressDir(dir); ok && p.ConversationID != "" {
			return p.ConversationID
		}
		if _, terminal := m.store.ExitCode(job.ID); terminal {
			// Re-read before giving up. The supervisor writes the progress file
			// before the sentinel, so a job that finished between the two reads
			// above has its id on disk even though the first read missed it;
			// returning "" here without looking again would discard it.
			if p, ok := jobstore.ReadProgressDir(dir); ok {
				return p.ConversationID
			}
			return ""
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			// The caller (the agy_run handler) has gone away, so stop polling rather
			// than parking it for the rest of the budget. The id is not lost: it stays
			// on disk in the progress file, so any later agy_status on this job returns
			// it once agy names the conversation, the same recovery an expired budget
			// relies on.
			return ""
		case <-time.After(conversationIDPoll):
		}
	}
}

// conversationLive reports whether some running job is already driving convID.
//
// The gate key alone cannot answer this. A fresh run keys on nothing (agy has
// not named its conversation yet at admission time), but the supervisor records
// that name in progress.json within about a second, and agy_run reports it to the
// caller. Without this scan a caller could take a still-running fresh run's id,
// pass it straight back as conversation_id, and be admitted under a conv key
// nobody holds, putting two agy sessions on one conversation: exactly the
// session-lock hang the gate exists to prevent.
//
// A scan is affordable here because it runs only on the continuation path, over
// jobs bounded by JobTTL. An unreadable job is skipped rather than treated as a
// match: refusing every continuation because one unrelated job dir is corrupt
// would be worse than the narrow race this closes.
func (m *Manager) conversationLive(convID string) (bool, error) {
	if convID == "" {
		// Every fresh job has an empty conversation id, so an empty query would
		// match all of them and refuse every run. Only the continuation path calls
		// this, but the guard belongs with the function rather than its caller.
		return false, nil
	}
	ids, err := m.store.List()
	if err != nil {
		// Fail closed, like admit and RestoreGate. Without a scan there is no way
		// to know whether the conversation is busy, and guessing "idle" admits the
		// second session this check exists to prevent.
		return false, fmt.Errorf("scan jobs to check whether conversation %s is already running: %w", convID, err)
	}
	for _, id := range ids {
		meta, err := m.store.Load(id)
		if err != nil {
			// One unreadable job dir must not refuse every continuation forever, so
			// this skips rather than failing closed; log it, because the skip is a
			// hole in the check and a silent one would be indistinguishable from a
			// conversation that really is idle.
			log.Printf("conversation check: skipping job %s with unreadable meta: %v", id, err)
			continue
		}
		if _, terminal := m.store.ExitCode(id); terminal {
			continue
		}
		if !m.processAlive(meta) {
			continue
		}
		if meta.ConversationID == convID {
			return true, nil
		}
		// A fresh run's id lives only in progress.json; meta never learns it.
		dir, err := m.store.Dir(id)
		if err != nil {
			log.Printf("conversation check: skipping job %s with unusable dir: %v", id, err)
			continue
		}
		if p, ok := jobstore.ReadProgressDir(dir); ok && p.ConversationID == convID {
			return true, nil
		}
	}
	return false, nil
}

// StartJob persists meta and spawns the detached supervisor.
func (m *Manager) StartJob(req StartRequest) (Job, error) {
	// conversation_id and continue_latest are mutually exclusive: continue_latest
	// resolves to a conversation id that would otherwise silently overwrite the
	// explicit one, and the precedence even flips depending on whether the cache
	// resolves. Reject the combination outright (before any platform or filesystem
	// work) so the caller gets a deterministic error instead of a confusing winner.
	if req.ContinueLatest && req.ConversationID != "" {
		return Job{}, fmt.Errorf("conversation_id and continue_latest are mutually exclusive: pass one or the other")
	}
	// Job supervision is implemented on Linux (process groups, /proc) and Windows
	// (Job Objects, OpenProcess). On other platforms refuse before doing any work, so
	// the failure is a clear error rather than a half-spawned job. stdio/HTTP serve,
	// list_models, and list_sessions still work everywhere.
	if !proc.Supported {
		return Job{}, proc.ErrUnsupported
	}
	// Resolve agy before anything reservable is taken. config.Resolve tolerates a
	// missing agy so the server can still serve introspection, which makes this
	// the first point that genuinely needs the binary; doing it after admit would
	// burn a concurrency slot and strand the conversation key on a run that cannot start.
	// The version gate lives here too: the whole job pipeline decodes agy's
	// stream-json events, so an agy that cannot emit them must be refused before
	// a job dir exists, not diagnosed from a confusing empty result later.
	agy, err := m.agyBinaryChecked(context.Background())
	if err != nil {
		return Job{}, err
	}
	id, err := newID()
	if err != nil {
		return Job{}, fmt.Errorf("generate job id: %w", err)
	}
	cwd := req.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			// An empty cwd would run agy in whatever directory this process
			// inherited, and would be persisted as the job's cwd. Fail instead.
			// (It has no bearing on serialization: keyFor reads only the
			// conversation id, so every fresh run keys on nothing regardless.)
			return Job{}, fmt.Errorf("determine working directory: %w", err)
		}
		cwd = wd
	}
	// Canonicalize the cwd once so the gate key, cache lookups, cmd.Dir, and
	// persisted meta all share one spelling (see normalizeCwd). Report the
	// pre-normalization input on failure (normalizeCwd returns "" on error).
	normalized, err := normalizeCwd(cwd)
	if err != nil {
		return Job{}, fmt.Errorf("normalize working directory %q: %w", cwd, err)
	}
	cwd = normalized

	// Resolve every value that feeds the gate key, agy args, and persisted meta
	// back into req, so all three derive from one normalized request; keeping a
	// resolved value in a separate local while req stays stale risks a later read
	// of req.Model/req.Timeout silently bypassing the default fallback.
	req.Cwd = cwd
	if req.Model == "" {
		req.Model = m.cfg.DefaultModel
	}
	// Reduce a whole `agy models` row to its id (see modelID). Applied after the
	// fallback above so it covers a configured AGY_MCP_DEFAULT_MODEL too, and
	// before the args and meta below, which are the two readers of req.Model.
	req.Model = modelID(req.Model)
	if req.Timeout <= 0 {
		req.Timeout = m.cfg.DefaultTimeout
	}

	// Resolve continue_latest to a concrete conversation id before computing the
	// gate key and args, so serialization, the agy --conversation flag, and the
	// returned conversation id are all deterministic and consistent.
	if req.ContinueLatest {
		if cid, ok := resolveLatest(m.cacheFile, cwd); ok {
			req.ConversationID = cid
		}
	}

	// Refuse a continuation of a conversation some running job already drives.
	// The gate covers two runs that both name the conversation up front; this
	// covers the case it cannot see, a still-running fresh run that agy has since
	// named. Checked before admission so a refusal burns no concurrency slot.
	if req.ConversationID != "" {
		live, err := m.conversationLive(req.ConversationID)
		if err != nil {
			return Job{}, err
		}
		if live {
			return Job{}, fmt.Errorf("another agy job is already running on conversation %s; wait for it to finish or start a fresh run instead", req.ConversationID)
		}
	}

	key := keyFor(req)
	outcome, err := m.admit(key)
	if err != nil {
		// The cross-process lock could not be established (e.g. the locks dir is
		// unwritable). Fail closed: starting without it could let a sibling process
		// run a conflicting same-key job and re-expose the session-lock hang.
		return Job{}, fmt.Errorf("acquire cross-process lock for this conversation: %w", err)
	}
	switch outcome {
	case acquireOK:
		// Slot and cross-process lock reserved; proceed to spawn below.
	case acquireKeyBusy:
		return Job{}, fmt.Errorf("another agy job is already running on conversation %s; wait for it to finish or start a fresh run instead", req.ConversationID)
	case acquireAtCap:
		return Job{}, fmt.Errorf("agy-mcp is at its concurrency cap of %d running job(s); retry once one finishes", m.gate.cap())
	default:
		// Fail closed: a future acquireOutcome must never fall through and start a
		// job without a reserved slot or key.
		return Job{}, fmt.Errorf("internal admission error: unknown acquire outcome %d", outcome)
	}

	args := buildAgyArgs(req)
	meta := jobstore.Meta{
		ID:             id,
		AgyPath:        agy,
		Args:           args,
		Cwd:            req.Cwd,
		Model:          req.Model,
		ConversationID: req.ConversationID,
		Prompt:         req.Prompt,
		StartedAt:      time.Now().UTC(),
		BootID:         readBootID(),
		Timeout:        req.Timeout,
	}
	if meta.BootID == "" {
		// boot_id was unreadable, so this job's PID cannot be pinned to a boot. While
		// boot_id stays unreadable (e.g. a restricted container) liveness still works
		// via the PID and start-time checks; but if boot_id becomes readable later
		// (notably after a reboot) this job reads as not-alive, so it cannot be
		// restored across a manager restart and a recycled PID is never mistaken for
		// it. Surface the unreadable boot_id since it degrades cross-boot liveness.
		log.Printf("job %s: kernel boot id unreadable; cross-boot liveness degraded for this job", id)
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		m.releaseKey(key)
		return Job{}, fmt.Errorf("create job store entry: %w", err)
	}

	cmd := exec.Command(m.cfg.SupervisorExe, "run-job", dir)
	cmd.Env = os.Environ()
	// Detach stdio: supervisor must not inherit the manager's stdout (the
	// JSON-RPC stream in stdio mode).
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// StartDetached puts the supervisor in its own group/job and starts it detached
	// so it survives the manager's death (init adoption on Linux; a non-kill-on-close
	// Job Object on Windows).
	if err := proc.StartDetached(cmd); err != nil {
		// No supervisor was spawned, so the just-created job dir is a never-started
		// orphan; remove it now rather than leaving it for a later GarbageCollect.
		_ = m.store.Remove(id)
		m.releaseKey(key)
		return Job{}, fmt.Errorf("spawn supervisor: %w", err)
	}
	// Capture a handle to the supervisor's process tree so the record-pid failure
	// path below can terminate it (and any agy it already spawned) as a unit.
	// killOnClose=false: closing the handle on manager exit must NOT kill the tree,
	// so the detached supervisor still outlives the manager.
	grp, err := proc.Track(cmd, false)
	if err != nil {
		// Started but untrackable (Track only fails before Start, which succeeded).
		// Fail closed: kill the supervisor, then reap it and clean up in the
		// background, releasing the gate key only once it has fully exited. Releasing
		// after the supervisor is gone (rather than synchronously here) keeps a
		// conflicting same-key run from starting while the dying agy still holds its
		// session lock, matching abortSpawn's teardown contract. Track failed, so
		// there is no group handle to terminate; the direct process Kill is all that
		// is available.
		_ = cmd.Process.Kill()
		go func() {
			_ = cmd.Wait()
			_ = m.store.Remove(id)
			m.releaseKey(key)
		}()
		return Job{}, fmt.Errorf("track supervisor: %w", err)
	}
	// Record the supervisor PID (for liveness and cancel) and its start time (so a
	// later process that recycles the same PID within this boot is not mistaken for
	// the supervisor) with an atomic rewrite, so the just-spawned supervisor never
	// reads a half-written meta.json.
	meta.PID = cmd.Process.Pid
	if ticks, ok := m.readStartTimeTicks(cmd.Process.Pid); ok {
		meta.StartTimeTicks = ticks
	} else if startTimeMandatory {
		// darwin has no /proc/comm-style liveness fallback, so processAlive fails
		// closed without a recorded start time; a job must never persist a 0. The
		// read is a local lookup of a child we just forked, so a failure (after the
		// retries in readStartTimeTicks) means it almost certainly died instantly.
		// Tear it down and release the gate.
		m.abortSpawn(cmd, grp, id, key)
		return Job{}, fmt.Errorf("record supervisor start time for pid %d", cmd.Process.Pid)
	}
	if err := m.store.UpdateMeta(meta); err != nil {
		// Without a persisted PID the supervisor would be untrackable
		// (uncancellable, and reported as not-alive). Fail closed: tear it down and
		// release the gate once it has fully exited (see abortSpawn).
		m.abortSpawn(cmd, grp, id, key)
		return Job{}, fmt.Errorf("record supervisor pid: %w", err)
	}
	// Wait for the supervisor in the background. cmd.Wait returns exactly when
	// the supervisor (and thus the job) exits, so this both reaps it (no zombie)
	// and releases the gate slot at the precise moment the job ends, with no
	// disk polling. The supervisor is detached and survives manager death; if
	// the manager dies first, init adopts and reaps it.
	go func() {
		_ = cmd.Wait()
		_ = grp.Close()
		m.releaseKey(key)
	}()

	// The conversation id is not resolved here. A continuation already carries it;
	// a fresh run is named by agy's init event a moment later, and waiting for that
	// is AwaitConversationID's job, paid only by the caller that reports the id in
	// its own response.
	return Job{ID: id, ConversationID: req.ConversationID, State: StateRunning}, nil
}

// abortSpawn tears down a supervisor that was started but cannot be recorded as
// a running job. It terminates the process group, then in the background reaps
// it and, only once it has fully exited (so nothing is still writing the job
// dir), removes the dir and releases the gate. Releasing only after the agy
// process group is gone keeps a conflicting same-key run from starting while the
// dying agy still holds its session lock. Both spawn-failure paths (start time
// unreadable on darwin, and UpdateMeta failure) share it so they cannot diverge.
func (m *Manager) abortSpawn(cmd *exec.Cmd, grp *proc.Group, id, key string) {
	_ = grp.Terminate(syscall.SIGTERM)
	go func() {
		_ = cmd.Wait()
		_ = grp.Close()
		_ = m.store.Remove(id)
		m.releaseKey(key)
	}()
}

// loadedJob is one job's meta from a single meta.json read, the shared input the
// GC and gate-restore decisions both need. Loading it once lets the fused startup
// pass (RestoreAndCollect) make both decisions without re-reading every job dir.
type loadedJob struct {
	id      string
	meta    jobstore.Meta
	metaErr error // non-nil if meta.json could not be loaded (missing, corrupt, or transient)
}

// loadJob reads one job's meta with a single store.Load.
func (m *Manager) loadJob(id string) loadedJob {
	meta, err := m.store.Load(id)
	return loadedJob{id: id, meta: meta, metaErr: err}
}

// RestoreAndCollect runs startup garbage collection and concurrency-gate
// restoration in a single job-store scan, halving the meta.json reads versus
// calling GarbageCollect and RestoreGate back to back (each previously loaded
// every job's meta). The single loadJob result is shared by both decisions; each
// still reads the exit_code sentinel itself, fresh, exactly as the two methods
// did standalone, so the sentinel's read count and freshness are unchanged. Each
// job's keep/remove and restore decisions are independent and mutually exclusive
// (GC only removes terminal or dead jobs; restore only re-occupies the gate for
// live ones), so one pass produces the same result as the two sequential scans it
// replaces.
//
// It fails closed like RestoreGate: if the jobs cannot be scanned the gate cannot
// be made safe, so it returns an error and startup should refuse rather than serve
// with an unrestored gate (a stricter contract than GarbageCollect alone, whose
// scan failure was previously only logged). The returned ids are the jobs GC
// removed (empty when JobTTL disables collection). Intended to run once at startup.
func (m *Manager) RestoreAndCollect() (removed []string, err error) {
	ids, err := m.store.List()
	if err != nil {
		return nil, fmt.Errorf("scan jobs for startup recovery: %w", err)
	}
	// Mirror GarbageCollect's JobTTL<=0 short-circuit: collection is disabled, but
	// the gate still must be restored, so the scan continues with GC skipped.
	gcEnabled := m.cfg.JobTTL > 0
	var cutoff time.Time
	if gcEnabled {
		cutoff = time.Now().Add(-m.cfg.JobTTL)
	}
	for _, id := range ids {
		l := m.loadJob(id)
		if gcEnabled && m.gcEvaluate(l, cutoff) {
			removed = append(removed, id)
			continue // removed; nothing to restore (a removed job was terminal or dead)
		}
		m.restoreEvaluate(l)
	}
	return removed, nil
}

// GarbageCollect removes jobs older than the configured TTL. A job whose
// supervisor is still alive (including one that survived a manager restart) is
// never removed, even past the TTL, so a live job is never deleted out from
// under its supervisor. A non-positive TTL disables collection.
func (m *Manager) GarbageCollect() ([]string, error) {
	if m.cfg.JobTTL <= 0 {
		return nil, nil
	}
	ids, err := m.store.List()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-m.cfg.JobTTL)
	var removed []string
	for _, id := range ids {
		if m.gcEvaluate(m.loadJob(id), cutoff) {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// gcEvaluate applies the keep-or-remove decision to one already-loaded job,
// removing its dir when collectable and reporting whether it did. cutoff is
// now-TTL. Callers must already have established JobTTL>0. It is the body of
// GarbageCollect's loop, factored out so the fused startup pass can reuse the one
// loadJob result without re-reading meta.json.
func (m *Manager) gcEvaluate(l loadedJob, cutoff time.Time) bool {
	if l.metaErr != nil {
		if errors.Is(l.metaErr, fs.ErrNotExist) {
			// meta.json is genuinely missing: an orphan from a crash between
			// Create's MkdirAll and its meta write, or a partial RemoveAll. It can
			// never be a live, attributable job (there is no PID or boot id to
			// check), so the old behavior of skipping it silently and forever let
			// orphans accumulate without bound. Reap it once it is older than the
			// TTL, judged by the dir mtime since there is no StartedAt; a fresh one
			// may be a job still mid-Create, so it is kept.
			if m.orphanExpired(l.id, cutoff) {
				if rerr := m.store.Remove(l.id); rerr != nil {
					log.Printf("GC: remove orphan job dir %s: %v", l.id, rerr)
					return false
				}
				return true
			}
			log.Printf("GC: orphan job dir %s (no meta.json) kept until older than the TTL", l.id)
			return false
		}
		// meta.json exists but could not be read or parsed: a transient error
		// (e.g. EMFILE/EIO) or a corrupt write. This is NOT an orphan and must not
		// be reaped by dir mtime: a valid long-running job's dir mtime is old
		// (writing to out/err does not bump the directory mtime), so reaping here
		// could delete a live job. Log and keep; a later sweep retries the Load.
		log.Printf("GC: job %s meta unreadable (not reaping, may be transient): %v", l.id, l.metaErr)
		return false
	}
	if !l.meta.StartedAt.Before(cutoff) {
		return false // too recent to collect (and so the exit_code sentinel is never read)
	}
	_, terminal := m.store.ExitCode(l.id)
	if !terminal {
		if m.processAlive(l.meta) {
			return false // still running; keep it
		}
		// Not alive and not terminal at the first read. The supervisor may have
		// written its sentinel and exited between the ExitCode and processAlive
		// checks above (the same race Status guards against); re-read once. If it
		// is now terminal the job just finished, so its result is freshly written
		// and unread; keep it this sweep and collect it on the next one (where the
		// first read sees the sentinel).
		//
		// A terminal job needs no equivalent guard. The supervisor writes
		// result.json before the exit-code sentinel, so once the sentinel exists
		// nothing is still writing the dir and it is safe to remove.
		if _, t := m.store.ExitCode(l.id); t {
			return false
		}
	}
	if err := m.store.Remove(l.id); err != nil {
		log.Printf("GC: remove expired job %s: %v", l.id, err)
		return false
	}
	return true
}

// orphanExpired reports whether the job dir for id, whose meta.json is missing,
// is older than the GC cutoff. It judges age by the directory's mtime because
// there is no StartedAt to read. A Dir or stat failure returns false, so a
// transient error never removes a dir that might still be in use.
func (m *Manager) orphanExpired(id string, cutoff time.Time) bool {
	dir, err := m.store.Dir(id)
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.ModTime().Before(cutoff)
}

// gcInterval returns how often a long-lived server should sweep finished jobs:
// half the job TTL, so a finished job is collected within ~1.5x its TTL, floored
// at one minute so a tiny configured TTL cannot spin the ticker. A non-positive
// TTL returns 0, which disables periodic collection (matching GarbageCollect's
// own JobTTL<=0 short-circuit).
func gcInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if d := ttl / 2; d > time.Minute {
		return d
	}
	return time.Minute
}

// RunPeriodicGCFromConfig runs periodic GC using the sweep interval derived from
// the configured JobTTL (see gcInterval). It blocks until ctx is cancelled, so
// callers run it in a goroutine; it is a no-op when JobTTL disables collection.
func (m *Manager) RunPeriodicGCFromConfig(ctx context.Context) {
	m.runPeriodicGC(ctx, gcInterval(m.cfg.JobTTL))
}

// runPeriodicGC sweeps finished jobs every interval until ctx is cancelled, so a
// long-running HTTP daemon does not accumulate finished job dirs between restarts.
// It reuses GarbageCollect, which never removes a still-alive job, so the ticker
// inherits that safety. It blocks; callers run it in a goroutine. A non-positive
// interval is a no-op.
func (m *Manager) runPeriodicGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if removed, err := m.GarbageCollect(); err != nil {
				log.Printf("periodic GC: %v", err)
			} else if len(removed) > 0 {
				log.Printf("periodic GC removed %d expired job(s)", len(removed))
			}
		}
	}
}

// reqFromMeta reconstructs the parts of a StartRequest that determine a job's
// gate key from its persisted meta. continue_latest is resolved into
// ConversationID at start time, so the conversation id alone reproduces the key
// the original run held.
//
// It used to re-normalize meta.Cwd as well, because the key was once derived
// from the directory and a legacy job's raw cwd would otherwise restore under a
// different key than a new same-directory run computed. Fresh runs stopped being
// keyed by directory when the conversation id started coming from agy's own
// stream, so keyFor reads nothing but ConversationID and the Abs plus
// EvalSymlinks (plus an os.Getwd for a relative cwd) ran once per job in the
// startup scan for a value nothing consumed.
func reqFromMeta(meta jobstore.Meta) StartRequest {
	return StartRequest{ConversationID: meta.ConversationID}
}

// RestoreGate re-acquires gate slots and keys for jobs whose detached supervisor
// survived a manager restart, so a new agy_run is serialized against them and the
// global cap accounts for them. Without this, a restored job is invisible to the
// gate and the cap could be bypassed, re-exposing the session-lock hang the gate
// prevents. The startup path uses RestoreAndCollect, which fuses this with
// GarbageCollect into one scan; this method remains for restore-only use and is
// exercised directly by tests.
//
// It fails closed: if the on-disk jobs cannot be scanned the gate cannot be made
// safe, so it returns an error and the caller should refuse to start rather than
// serve with an unrestored gate.
func (m *Manager) RestoreGate() error {
	ids, err := m.store.List()
	if err != nil {
		return fmt.Errorf("scan jobs to restore concurrency gate: %w", err)
	}
	for _, id := range ids {
		m.restoreEvaluate(m.loadJob(id))
	}
	return nil
}

// restoreEvaluate re-occupies the gate for one already-loaded job whose detached
// supervisor outlived a manager restart. It is the body of RestoreGate's loop,
// factored out so the fused startup pass can reuse the one loadJob result without
// re-reading meta.json. It reads the exit_code sentinel itself, fresh, so a job
// that finished since the meta was loaded is not restored into the gate.
func (m *Manager) restoreEvaluate(l loadedJob) {
	if l.metaErr != nil {
		// A job whose meta cannot be read cannot have its gate key recomputed or
		// its liveness checked, so its slot cannot be restored. Log it rather than
		// skipping silently: if such a job's supervisor is somehow still alive its
		// gate is not held, the one gap in this method's fail-closed contract.
		// GarbageCollect reaps the dir once it is older than the TTL.
		log.Printf("restore gate: skipping job %s with unreadable meta: %v", l.id, l.metaErr)
		return
	}
	if _, terminal := m.store.ExitCode(l.id); terminal {
		return // already finished; nothing to hold
	}
	if !m.processAlive(l.meta) {
		return // supervisor gone; GarbageCollect will reap it
	}
	// forceAdmit counts the job and holds its key unconditionally: a restored job is
	// already running, so it must be tracked even past the cap (otherwise a new
	// same-key run could start once a slot frees and run concurrently with it, the
	// bypass this method prevents). It also best-effort re-takes the cross-process
	// lock so sibling processes are blocked again across the restart. A false return
	// means another restored job already holds this key, so it is already watched.
	key := keyFor(reqFromMeta(l.meta))
	if m.forceAdmit(key) {
		m.watchRestored(l.meta, key)
	}
}

// watchRestored releases a restored job's gate key once its detached supervisor
// exits. A restored job is not a child of this manager, so there is no cmd.Wait to
// release on; the watcher polls liveness instead. It only covers jobs that
// outlived a restart, so the polling cost is bounded.
func (m *Manager) watchRestored(meta jobstore.Meta, key string) {
	interval := m.restoredPollInterval
	dead := func() bool {
		if _, terminal := m.store.ExitCode(meta.ID); terminal {
			return true
		}
		return !m.processAlive(meta)
	}
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
		// The supervisor records the conversation id itself, so a restored job
		// needs nothing but its gate key released once the supervisor is gone.
		m.releaseKey(key)
	}()
}

// agy CLI flags, shared with tests so the flag strings cannot drift between
// the arg builder and its assertions.
const (
	dangerouslySkipPermissionsFlag = "--dangerously-skip-permissions"
	printTimeoutFlag               = "--print-timeout"
	modelFlag                      = "--model"
	effortFlag                     = "--effort"
	modeFlag                       = "--mode"
	agentFlag                      = "--agent"
	sandboxFlag                    = "--sandbox"
	addDirFlag                     = "--add-dir"
	conversationFlag               = "--conversation"
	jsonSchemaFlag                 = "--json-schema"
	outputFormatFlag               = "--output-format"
	disableSlashCommandsFlag       = "--disable-slash-commands"
	// streamJSONFormat is the only format agy-mcp drives. It is what makes the
	// conversation id observable mid-run (the init event carries it) and what
	// leaves an interrupted run with recoverable text; the single-object `json`
	// format emits nothing until the run completes, so neither would work.
	streamJSONFormat = "stream-json"
)

func buildAgyArgs(req StartRequest) []string {
	args := []string{
		dangerouslySkipPermissionsFlag,
		printTimeoutFlag, req.Timeout.String(),
		outputFormatFlag, streamJSONFormat,
		// Keep caller prompts literal. Per agy's changelog, 1.1.9 added slash-command
		// and skill expansion to print mode together with --disable-slash-commands as
		// its opt-out, so without this a prompt whose leading token starts with "/"
		// (a path, a leading-slash sentence, or any caller text that happens to begin
		// with "/") is reinterpreted as a skill or command. The flag is functional in
		// headless -p, not merely accepted: the changelog introduces it as the opt-out
		// for that exact print-mode feature, unlike --effort which was accepted but a
		// no-op in -p until 1.1.10. Passing it on every run makes prompt semantics mean
		// what the caller wrote, independent of the installed agy version. Always
		// available because the agy floor (agyver.Required) is at or above the 1.1.9
		// that introduced the flag.
		disableSlashCommandsFlag,
	}
	if req.Model != "" {
		args = append(args, modelFlag, req.Model)
	}
	if req.Effort != "" {
		args = append(args, effortFlag, req.Effort)
	}
	if req.Mode != "" {
		args = append(args, modeFlag, req.Mode)
	}
	if req.Agent != "" {
		args = append(args, agentFlag, req.Agent)
	}
	if req.Sandbox {
		args = append(args, sandboxFlag)
	}
	for _, d := range req.Dirs {
		args = append(args, addDirFlag, d)
	}
	if req.ConversationID != "" {
		args = append(args, conversationFlag, req.ConversationID)
	}
	if req.JSONSchema != "" {
		args = append(args, jsonSchemaFlag, req.JSONSchema)
	}
	args = append(args, "-p", req.Prompt)
	return args
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Time prefix keeps IDs roughly sortable.
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}
