// Package manager owns agy job lifecycle: spawning, status, cancel, and the
// model/session queries. It is transport-agnostic.
package manager

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// Manager coordinates jobs backed by an on-disk store.
type Manager struct {
	cfg       config.Config
	store     jobStore // *jobstore.Store in production; an interface so tests can inject failures
	gate      *gate    // serializes conflicting jobs and caps total concurrency
	cacheFile string   // agy conversation cache (last_conversations.json); injectable for tests

	// pendingCaptures holds the job ids of fresh runs whose conversation-id
	// capture is armed but not yet settled (the post-exit capture attempt has
	// not finished). Keyed by job id; values are struct{}.
	pendingCaptures sync.Map

	// settledCapture memoizes job ids whose lazy capture is permanently over
	// (no id is coming): either the run is long past its timeout with no cache
	// change, or a later same-cwd run made attribution unsafe. Settled jobs
	// stop re-reading the cache on every Status poll.
	settledMu      sync.Mutex
	settledCapture map[string]struct{}

	// Timing for the fresh-run conversation-id capture and the restored-job
	// liveness watcher. Fields (not package globals) so tests stay isolated and can
	// run in parallel. agy's conversation cache is flushed by a separate daemon that
	// can lag the foreground agy exit, so the capture retries briefly. Verified
	// against agy 1.0.6: agy rewrites last_conversations.json in place (O_TRUNC, no
	// file lock), so a concurrent read can be torn; loadCache reports torn reads
	// as errors, capture treats them as "no capture yet" and this retry loop
	// re-reads, and StartJob disables capture when the pre-run snapshot itself is
	// unreadable, so no mutex is needed. agy also ignores a caller-supplied fresh --conversation UUID and
	// mints its own, which is why the id must be captured by diffing the cache
	// rather than generated and passed in.
	captureBudget        time.Duration
	capturePoll          time.Duration
	restoredPollInterval time.Duration
}

// New constructs a Manager.
func New(c config.Config) *Manager {
	return &Manager{
		cfg:                  c,
		store:                jobstore.New(c.StateDir),
		gate:                 newGate(c.MaxConcurrency),
		cacheFile:            cmp.Or(c.ConversationCacheFile, agyCachePath()),
		captureBudget:        2 * time.Second,
		capturePoll:          100 * time.Millisecond,
		restoredPollInterval: 2 * time.Second,
		settledCapture:       make(map[string]struct{}),
	}
}

// StartRequest describes a run to start.
type StartRequest struct {
	Prompt         string
	Model          string   // optional; falls back to cfg.DefaultModel
	Dirs           []string // repeated --add-dir
	ConversationID string   // optional; --conversation <id>
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

// captureFreshConversationID records the conversation id agy created for a fresh
// run by diffing the cwd's cache entry against the pre-run snapshot, and persists
// it to meta so Status reports it. It is a no-op once an id is already known.
//
// The caller must invoke this while still holding the run's gate key: the cwd key
// serializes same-cwd fresh runs, so no other run can overwrite the cache entry
// between the snapshot and this capture. A torn or missing cache read yields no
// capture (the run simply reports no id), never a misattribution.
func (m *Manager) captureFreshConversationID(meta *jobstore.Meta) {
	if meta.ConversationID != "" || meta.CaptureDisabled {
		return
	}
	deadline := time.Now().Add(m.captureBudget)
	for {
		if id, ok := captureNewUUID(m.cacheFile, meta.Cwd, meta.CwdUUIDBefore); ok {
			final, err := m.store.SetConversationID(meta.ID, id)
			if err != nil {
				log.Printf("agy-mcp: persist captured conversation id for job %s: %v", meta.ID, err)
				final = id
			}
			meta.ConversationID = final
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(m.capturePoll)
	}
}

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

// CapturePending reports whether a fresh run's conversation-id capture has not
// yet settled in this process: capture was armed at start (or restore) and the
// post-exit capture attempt has not finished. Pollers use it to distinguish
// "done, id still being captured" from "done, no id is coming".
func (m *Manager) CapturePending(id string) bool {
	_, ok := m.pendingCaptures.Load(id)
	return ok
}

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

// StartJob persists meta and spawns the detached supervisor.
func (m *Manager) StartJob(req StartRequest) (Job, error) {
	// Job supervision needs process groups and /proc, which only exist on Linux. On
	// other platforms refuse before doing any work, so the failure is a clear error
	// rather than a half-spawned job. stdio/HTTP serve, list_models, and list_sessions
	// still work everywhere.
	if !platformSupported {
		return Job{}, errPlatformUnsupported
	}
	id, err := newID()
	if err != nil {
		return Job{}, err
	}
	cwd := req.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	model := req.Model
	if model == "" {
		model = m.cfg.DefaultModel
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = m.cfg.DefaultTimeout
	}

	req.Cwd = cwd

	// Resolve continue_latest to a concrete conversation id before computing the
	// gate key and args, so serialization, the agy --conversation flag, and the
	// returned conversation id are all deterministic and consistent.
	if req.ContinueLatest {
		if cid, ok := resolveLatest(m.cacheFile, cwd); ok {
			req.ConversationID = cid
		}
	}

	key := keyFor(req)
	if !m.gate.tryAcquire(key) {
		return Job{}, fmt.Errorf("a conflicting agy job for this conversation or directory is already running")
	}

	args := buildAgyArgs(req, model, timeout)
	meta := jobstore.Meta{
		ID:             id,
		AgyPath:        m.cfg.AgyPath,
		Args:           args,
		Cwd:            cwd,
		Model:          model,
		ConversationID: req.ConversationID,
		Prompt:         req.Prompt,
		StartedAt:      time.Now().UTC(),
		BootID:         readBootID(),
		Timeout:        timeout,
	}
	// Whenever the run has no resolved conversation id (a fresh run, or a
	// continue_latest that found no prior conversation), agy will create a new
	// conversation. Snapshot the cwd's current conversation id so a later diff
	// can capture the one agy creates.
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
	dir, err := m.store.Create(meta)
	if err != nil {
		m.pendingCaptures.Delete(id)
		m.gate.release(key)
		return Job{}, err
	}

	cmd := exec.Command(m.cfg.SupervisorExe, "run-job", dir)
	cmd.Env = os.Environ()
	setProcessGroup(cmd)
	// Detach stdio: supervisor must not inherit the manager's stdout (the
	// JSON-RPC stream in stdio mode).
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		// No supervisor was spawned, so the just-created job dir is a never-started
		// orphan; remove it now rather than leaving it for a later GarbageCollect.
		_ = m.store.Remove(id)
		m.pendingCaptures.Delete(id)
		m.gate.release(key)
		return Job{}, fmt.Errorf("spawn supervisor: %w", err)
	}
	// Record the supervisor PID (for liveness and cancel) and its start time (so a
	// later process that recycles the same PID within this boot is not mistaken for
	// the supervisor) with an atomic rewrite, so the just-spawned supervisor never
	// reads a half-written meta.json.
	meta.PID = cmd.Process.Pid
	if ticks, ok := readStartTimeTicks(cmd.Process.Pid); ok {
		meta.StartTimeTicks = ticks
	}
	if err := m.store.UpdateMeta(meta); err != nil {
		// Without a persisted PID the supervisor would be untrackable
		// (uncancellable, and reported as not-alive). Fail closed: terminate it,
		// then once it has fully exited (so nothing is still writing the job dir)
		// remove the dir and release the gate. Releasing only after the agy process
		// group is gone keeps a conflicting same-key run from starting while the
		// dying agy still holds its session lock.
		_ = terminateGroup(cmd.Process.Pid)
		go func() {
			_ = cmd.Wait()
			_ = m.store.Remove(id)
			m.pendingCaptures.Delete(id)
			m.gate.release(key)
		}()
		return Job{}, fmt.Errorf("record supervisor pid: %w", err)
	}
	// Wait for the supervisor in the background. cmd.Wait returns exactly when
	// the supervisor (and thus the job) exits, so this both reaps it (no zombie)
	// and releases the gate slot at the precise moment the job ends, with no
	// disk polling. The supervisor is detached and survives manager death; if
	// the manager dies first, init adopts and reaps it.
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

	return Job{ID: id, ConversationID: req.ConversationID, State: StateRunning}, nil
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
		meta, err := m.store.Load(id)
		if err != nil {
			continue
		}
		if !meta.StartedAt.Before(cutoff) {
			continue // too recent to collect
		}
		if _, terminal := m.store.ExitCode(id); !terminal && m.processAlive(meta) {
			continue // still running; keep it
		}
		if err := m.store.Remove(id); err == nil {
			removed = append(removed, id)
		}
	}
	return removed, nil
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
				log.Printf("agy-mcp: periodic GC: %v", err)
			} else if len(removed) > 0 {
				log.Printf("agy-mcp: periodic GC removed %d expired job(s)", len(removed))
			}
		}
	}
}

// reqFromMeta reconstructs the parts of a StartRequest that determine a job's
// gate key from its persisted meta. continue_latest is resolved into
// ConversationID at start time, so ConversationID + Cwd reproduce the same key
// the original run held.
func reqFromMeta(meta jobstore.Meta) StartRequest {
	return StartRequest{ConversationID: meta.ConversationID, Cwd: meta.Cwd}
}

// RestoreGate re-acquires gate slots and keys for jobs whose detached supervisor
// survived a manager restart, so a new agy_run is serialized against them and the
// global cap accounts for them. Without this, a restored job is invisible to the
// gate and the cap could be bypassed, re-exposing the session-lock hang the gate
// prevents. Intended to run once at startup, after GarbageCollect.
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
		meta, err := m.store.Load(id)
		if err != nil {
			continue
		}
		if _, terminal := m.store.ExitCode(id); terminal {
			continue // already finished; nothing to hold
		}
		if !m.processAlive(meta) {
			continue // supervisor gone; GarbageCollect will reap it
		}
		// forceAcquire counts the job and holds its key unconditionally: a restored
		// job is already running, so it must be tracked even past the cap (otherwise a
		// new same-key run could start once a slot frees and run concurrently with it,
		// the bypass this method prevents). A false return means another restored job
		// already holds this key, so it is already watched.
		key := keyFor(reqFromMeta(meta))
		if m.gate.forceAcquire(key) {
			if meta.ConversationID == "" && !meta.CaptureDisabled {
				// Mirror StartJob: arm the capture so pollers can tell this
				// restored fresh run's id is still being settled.
				m.pendingCaptures.Store(meta.ID, struct{}{})
			}
			m.watchRestored(meta, key)
		}
	}
	return nil
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
}

func buildAgyArgs(req StartRequest, model string, timeout time.Duration) []string {
	args := []string{"--dangerously-skip-permissions", "--print-timeout", timeout.String()}
	if model != "" {
		args = append(args, "--model", model)
	}
	for _, d := range req.Dirs {
		args = append(args, "--add-dir", d)
	}
	if req.ConversationID != "" {
		args = append(args, "--conversation", req.ConversationID)
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
