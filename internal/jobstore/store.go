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

// Meta describes a job. The identity fields are set at creation; PID,
// StartTimeTicks, and ConversationID are filled in afterward by atomic rewrites
// (UpdateMeta / SetConversationID).
type Meta struct {
	ID             string    `json:"id"`
	AgyPath        string    `json:"agy_path"`
	Args           []string  `json:"args"`
	Cwd            string    `json:"cwd"`
	Model          string    `json:"model,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Prompt         string    `json:"prompt"`
	StartedAt      time.Time `json:"started_at"`
	PID            int       `json:"pid"`
	StartTimeTicks uint64    `json:"start_time_ticks,omitempty"` // supervisor /proc start time; 0 = unknown
	BootID         string    `json:"boot_id"`
	CwdUUIDBefore  string    `json:"cwd_uuid_before,omitempty"`
	// CaptureDisabled marks a fresh run whose pre-run cache snapshot could not
	// be read: without a trustworthy snapshot a post-run cache diff cannot be
	// attributed safely, so conversation-id capture is skipped for this job.
	CaptureDisabled bool          `json:"capture_disabled,omitempty"`
	Timeout         time.Duration `json:"timeout,omitempty"`
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
	// mu serializes every meta rewrite (UpdateMeta and SetConversationID). It is
	// what lets SetConversationID's read-modify-write be atomic: it holds mu across
	// the Load and the rewrite, and because UpdateMeta also takes mu a concurrent
	// UpdateMeta cannot land between the two and be clobbered.
	mu sync.Mutex
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
	// 0700: job dirs hold prompts and full agy output (which often embed source
	// code), so they must not be readable by other users on a multi-user host.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Write meta.json with the same temp+rename pattern UpdateMeta uses, so a crash
	// mid-write can never leave a torn or zero-length meta.json that Load fails to
	// parse (which would orphan the dir). A unique id is being created, so no other
	// writer touches this dir and no lock is needed.
	if err := writeMetaAtomic(dir, m); err != nil {
		// Remove the just-created dir so a failed Create leaves no orphan: a dir
		// without a readable meta.json is reaped only after the TTL by GarbageCollect.
		_ = os.RemoveAll(dir)
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

// UpdateMeta atomically rewrites a job's meta.json. It takes s.mu so every meta
// rewrite is serialized: a concurrent reader (such as the freshly spawned
// supervisor) never observes a partially written file, and SetConversationID's
// read-modify-write cannot be clobbered by a concurrent UpdateMeta landing
// between its Load and its rewrite.
func (s *Store) UpdateMeta(m Meta) error {
	if !validJobID(m.ID) {
		return ErrInvalidID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateMetaLocked(m)
}

// updateMetaLocked performs the atomic rewrite. The caller must hold s.mu. It is
// the shared body of UpdateMeta (which acquires the lock) and SetConversationID
// (which already holds it), so the two never deadlock by re-locking.
func (s *Store) updateMetaLocked(m Meta) error {
	return writeMetaAtomic(s.jobDir(m.ID), m)
}

// writeMetaAtomic writes m to dir/meta.json by writing a uniquely-named temp file
// and renaming it into place, so a reader never observes a partially written file
// and a crash mid-write leaves either the old meta.json or none, never a torn one.
// It assumes the caller has validated m.ID and that dir already exists.
func writeMetaAtomic(dir string, m Meta) error {
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
	// Flush the data to disk before the rename so a crash cannot leave a renamed
	// but zero-length meta.json, which would orphan the job (Load fails, GC skips).
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// 0600: meta.json records the prompt and cwd; keep it owner-only. os.CreateTemp
	// already makes the temp 0600, so this only guards against a future mode change.
	if err := os.Chmod(tmpName, 0o600); err != nil {
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
	// Already holding s.mu, so rewrite via the locked variant; calling UpdateMeta
	// here would re-acquire s.mu and deadlock.
	if err := s.updateMetaLocked(m); err != nil {
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
// Like the other Store methods it guards the id, so a malicious client-supplied
// job_id can never produce a path that escapes the store root, even for a future
// caller that does not Load (and thus validate) the id first.
func (s *Store) Dir(id string) (string, error) {
	if !validJobID(id) {
		return "", ErrInvalidID
	}
	return s.jobDir(id), nil
}

// WriteExitCode writes the completion sentinel.
func (s *Store) WriteExitCode(id string, code int) error {
	if !validJobID(id) {
		return ErrInvalidID
	}
	return os.WriteFile(filepath.Join(s.jobDir(id), "exit_code"), []byte(strconv.Itoa(code)), 0o600)
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
