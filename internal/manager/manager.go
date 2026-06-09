// Package manager owns agy job lifecycle: spawning, status, cancel, and the
// model/session queries. It is transport-agnostic.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// Manager coordinates jobs backed by an on-disk store.
type Manager struct {
	cfg       config.Config
	store     *jobstore.Store
	gate      *gate  // serializes conflicting jobs and caps total concurrency
	cacheFile string // agy conversation cache (last_conversations.json); injectable for tests

	// Timing for the fresh-run conversation-id capture and the restored-job
	// liveness watcher. Fields (not package globals) so tests stay isolated and can
	// run in parallel. agy's conversation cache is flushed by a separate daemon that
	// can lag the foreground agy exit, so the capture retries briefly. Verified
	// against agy 1.0.6: agy rewrites last_conversations.json in place (O_TRUNC, no
	// file lock), so a concurrent read can be torn; loadCache tolerates that (an
	// unparsable read yields no capture) and this retry loop re-reads, so no mutex
	// is needed. agy also ignores a caller-supplied fresh --conversation UUID and
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
		cacheFile:            agyCachePath(),
		captureBudget:        2 * time.Second,
		capturePoll:          100 * time.Millisecond,
		restoredPollInterval: 2 * time.Second,
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
	if meta.ConversationID != "" {
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
// from the cache when the completion goroutine never ran (the manager was
// restarted while the job ran). It returns an already-known id unchanged. Unlike
// the completion-goroutine capture, no gate key is held here, so a same-cwd run
// that started in between may have overwritten the cache entry; in that case
// nothing is captured and the run reports no id, exactly as before this change.
func (m *Manager) lazyCaptureConversationID(meta jobstore.Meta) string {
	if meta.ConversationID != "" {
		return meta.ConversationID
	}
	id, ok := captureNewUUID(m.cacheFile, meta.Cwd, meta.CwdUUIDBefore)
	if !ok {
		return ""
	}
	final, err := m.store.SetConversationID(meta.ID, id)
	if err != nil {
		log.Printf("agy-mcp: persist captured conversation id for job %s: %v", meta.ID, err)
		return id // best-effort: report what we captured even if the persist failed
	}
	return final
}

// StartJob persists meta and spawns the detached supervisor.
func (m *Manager) StartJob(req StartRequest) (Job, error) {
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
		meta.CwdUUIDBefore = snapshotCwd(m.cacheFile, cwd)
	}
	dir, err := m.store.Create(meta)
	if err != nil {
		m.gate.release(key)
		return Job{}, err
	}

	cmd := exec.Command(m.cfg.SupervisorExe, "run-job", dir)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Detach stdio: supervisor must not inherit the manager's stdout (the
	// JSON-RPC stream in stdio mode).
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		// No supervisor was spawned, so the just-created job dir is a never-started
		// orphan; remove it now rather than leaving it for a later GarbageCollect.
		_ = m.store.Remove(id)
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
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		go func() {
			_ = cmd.Wait()
			_ = m.store.Remove(id)
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
