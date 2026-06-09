// Package jobstore persists agy-mcp job state to disk so jobs survive a
// manager restart.
package jobstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Meta describes a job. It is written once at creation and is immutable.
type Meta struct {
	ID             string        `json:"id"`
	AgyPath        string        `json:"agy_path"`
	Args           []string      `json:"args"`
	Cwd            string        `json:"cwd"`
	Model          string        `json:"model,omitempty"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Prompt         string        `json:"prompt"`
	StartedAt      time.Time     `json:"started_at"`
	PID            int           `json:"pid"`
	BootID         string        `json:"boot_id"`
	CwdUUIDBefore  string        `json:"cwd_uuid_before,omitempty"`
	Timeout        time.Duration `json:"timeout,omitempty"`
}

// Exit-code sentinels the supervisor writes to the exit_code file and the
// manager interprets when deriving status. The 128+signal values follow the
// shell convention; 124 follows GNU timeout's convention for a timed-out
// command.
const (
	ExitTimeout   = 124 // hard timeout fired; the agy process group was terminated
	ExitSpawnFail = 127 // the supervisor could not start agy
	ExitSIGINT    = 130 // agy exited via SIGINT (128+2); treated as a cancel
	ExitSIGTERM   = 143 // agy terminated by SIGTERM (128+15); cancel or timeout kill
)

// Store is a directory-backed collection of jobs.
type Store struct{ root string }

// New returns a Store rooted at dir/jobs.
func New(dir string) *Store { return &Store{root: filepath.Join(dir, "jobs")} }

func (s *Store) jobDir(id string) string { return filepath.Join(s.root, id) }

// Create writes meta.json for a new job and returns its directory.
func (s *Store) Create(m Meta) (string, error) {
	dir := s.jobDir(m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

// Load reads a job's Meta.
func (s *Store) Load(id string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), "meta.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// UpdateMeta atomically rewrites a job's meta.json by writing a temp file and
// renaming it into place, so a concurrent reader (such as the freshly spawned
// supervisor) never observes a partially written file.
func (s *Store) UpdateMeta(m Meta) error {
	dir := s.jobDir(m.ID)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "meta.json"))
}

// Remove deletes a job's directory and everything in it.
func (s *Store) Remove(id string) error { return os.RemoveAll(s.jobDir(id)) }

// Dir returns the on-disk directory for a job (out, err, exit_code live here).
func (s *Store) Dir(id string) string { return s.jobDir(id) }

// WriteExitCode writes the completion sentinel.
func (s *Store) WriteExitCode(id string, code int) error {
	return os.WriteFile(filepath.Join(s.jobDir(id), "exit_code"), []byte(strconv.Itoa(code)), 0o644)
}

// ExitCode returns the recorded exit code and whether it is present.
func (s *Store) ExitCode(id string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), "exit_code"))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return code, true
}

// List returns all known job IDs.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
