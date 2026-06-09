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
	"sync"
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

// ErrInvalidID is returned when a job id is not a safe path segment.
var ErrInvalidID = errors.New("invalid job id")

// Store is a directory-backed collection of jobs.
type Store struct {
	root string
	mu   sync.Mutex // serializes SetConversationID's read-modify-write
}

// New returns a Store rooted at dir/jobs.
func New(dir string) *Store { return &Store{root: filepath.Join(dir, "jobs")} }

func (s *Store) jobDir(id string) string { return filepath.Join(s.root, id) }

// validJobID reports whether id is a safe single path segment, with no path
// separators or parent-directory traversal, so it can never escape the store
// root when joined into a filesystem path. Server-generated ids always pass;
// this guards against a malicious client-supplied job_id reaching the store.
func validJobID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	return filepath.Base(id) == id
}

// Create writes meta.json for a new job and returns its directory.
func (s *Store) Create(m Meta) (string, error) {
	if !validJobID(m.ID) {
		return "", ErrInvalidID
	}
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
	if !validJobID(id) {
		return m, ErrInvalidID
	}
	b, err := os.ReadFile(filepath.Join(s.jobDir(id), "meta.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// UpdateMeta atomically rewrites a job's meta.json by writing a uniquely-named
// temp file and renaming it into place, so a concurrent reader (such as the
// freshly spawned supervisor) never observes a partially written file, and two
// concurrent writers never corrupt a shared temp file.
func (s *Store) UpdateMeta(m Meta) error {
	if !validJobID(m.ID) {
		return ErrInvalidID
	}
	dir := s.jobDir(m.ID)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "meta-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "meta.json")); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// SetConversationID persists convID as the job's conversation id, but only when
// it is currently unset: it reloads the latest meta and rewrites just that field,
// so it cannot clobber a concurrent meta update, and a second caller that races to
// capture the same job is a no-op. It returns the effective conversation id (the
// existing one if already set, otherwise convID).
func (s *Store) SetConversationID(id, convID string) (string, error) {
	// Serialize the read-modify-write so two callers racing to capture the same
	// job cannot both observe an empty id and have the later write win (TOCTOU).
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.Load(id)
	if err != nil {
		return "", err
	}
	if m.ConversationID != "" {
		return m.ConversationID, nil
	}
	m.ConversationID = convID
	if err := s.UpdateMeta(m); err != nil {
		return "", err
	}
	return convID, nil
}

// Remove deletes a job's directory and everything in it.
func (s *Store) Remove(id string) error {
	if !validJobID(id) {
		return ErrInvalidID
	}
	return os.RemoveAll(s.jobDir(id))
}

// Dir returns the on-disk directory for a job (out, err, exit_code live here).
func (s *Store) Dir(id string) string { return s.jobDir(id) }

// WriteExitCode writes the completion sentinel.
func (s *Store) WriteExitCode(id string, code int) error {
	if !validJobID(id) {
		return ErrInvalidID
	}
	return os.WriteFile(filepath.Join(s.jobDir(id), "exit_code"), []byte(strconv.Itoa(code)), 0o644)
}

// ExitCode returns the recorded exit code and whether it is present.
func (s *Store) ExitCode(id string) (int, bool) {
	if !validJobID(id) {
		return 0, false
	}
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
