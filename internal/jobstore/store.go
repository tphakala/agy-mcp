// Package jobstore persists agy-mcp job state to disk so jobs survive a
// manager restart.
package jobstore

import (
	"encoding/json"
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
	return m, json.Unmarshal(b, &m)
}

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
	if os.IsNotExist(err) {
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

// GC removes finished jobs whose StartedAt is older than ttl. Returns removed IDs.
func (s *Store) GC(ttl time.Duration) ([]string, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-ttl)
	var removed []string
	for _, id := range ids {
		m, err := s.Load(id)
		if err != nil {
			continue
		}
		if m.StartedAt.Before(cutoff) {
			if err := os.RemoveAll(s.jobDir(id)); err == nil {
				removed = append(removed, id)
			}
		}
	}
	return removed, nil
}
