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
	dir, err := m.store.Create(meta)
	if err != nil {
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
		return Job{}, fmt.Errorf("spawn supervisor: %w", err)
	}
	// Record the supervisor PID for liveness/cancel, then release it.
	meta.PID = cmd.Process.Pid
	if b, e := jobMetaBytes(meta); e == nil {
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
	}
	_ = cmd.Process.Release()

	return Job{ID: id, ConversationID: req.ConversationID, State: "running"}, nil
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
