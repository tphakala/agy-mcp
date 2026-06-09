// Package manager owns agy job lifecycle: spawning, status, cancel, and the
// model/session queries. It is transport-agnostic.
package manager

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// Manager coordinates jobs backed by an on-disk store.
type Manager struct {
	cfg   config.Config
	store *jobstore.Store
	gate  *gate // concurrency control (Task 8)
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
	ContinueLatest bool     // resolve cwd's latest conversation before run (Task 11)
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
	}
	// For a genuinely fresh run (no conversation, no continue), snapshot the
	// cwd's current conversation id so a later diff can capture the new
	// conversation agy creates.
	if req.ConversationID == "" && !req.ContinueLatest {
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
	// Record the supervisor PID for liveness/cancel, then release it.
	meta.PID = cmd.Process.Pid
	if b, e := jobMetaBytes(meta); e == nil {
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
	}
	_ = cmd.Process.Release()

	// The gate slot is released by a background watchdog when the job reaches a
	// terminal state (or after a bounded safety window).
	go m.releaseWhenDone(key, id, timeout)

	return Job{ID: id, ConversationID: req.ConversationID, State: "running"}, nil
}

func (m *Manager) releaseWhenDone(key, id string, timeout time.Duration) {
	deadline := time.Now().Add(timeout + time.Minute)
	for time.Now().Before(deadline) {
		if st, err := m.Status(id); err == nil {
			switch st.State {
			case "done", "failed", "cancelled":
				m.gate.release(key)
				return
			}
		}
		time.Sleep(time.Second)
	}
	m.gate.release(key) // safety release after the watchdog window
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

func jobMetaBytes(m jobstore.Meta) ([]byte, error) {
	return jsonMarshalIndent(m)
}
