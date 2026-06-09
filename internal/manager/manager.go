// Package manager owns agy job lifecycle: spawning, status, cancel, and the
// model/session queries. It is transport-agnostic.
package manager

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// Manager coordinates jobs backed by an on-disk store.
type Manager struct {
	cfg   config.Config
	store *jobstore.Store
	gate  *gate // serializes conflicting jobs and caps total concurrency
}

// New constructs a Manager.
func New(c config.Config) *Manager {
	return &Manager{
		cfg:   c,
		store: jobstore.New(c.StateDir),
		gate:  newGate(c.MaxConcurrency),
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
		if cid, ok := resolveLatest(agyCachePath(), cwd); ok {
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
		meta.CwdUUIDBefore = snapshotCwd(agyCachePath(), cwd)
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
		m.gate.release(key)
		return Job{}, fmt.Errorf("spawn supervisor: %w", err)
	}
	// Record the supervisor PID (for liveness and cancel) with an atomic rewrite,
	// so the just-spawned supervisor never reads a half-written meta.json.
	meta.PID = cmd.Process.Pid
	if err := m.store.UpdateMeta(meta); err != nil {
		// Without a persisted PID the supervisor would be untrackable
		// (uncancellable, and reported as not-alive). Fail closed: terminate it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		go func() { _ = cmd.Wait() }()
		m.gate.release(key)
		return Job{}, fmt.Errorf("record supervisor pid: %w", err)
	}
	// Wait for the supervisor in the background. cmd.Wait returns exactly when
	// the supervisor (and thus the job) exits, so this both reaps it (no zombie)
	// and releases the gate slot at the precise moment the job ends, with no
	// disk polling. The supervisor is detached and survives manager death; if
	// the manager dies first, init adopts and reaps it.
	go func() {
		_ = cmd.Wait()
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
