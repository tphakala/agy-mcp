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
	ID             string        `json:"id"`
	AgyPath        string        `json:"agy_path"`
	Args           []string      `json:"args"`
	Cwd            string        `json:"cwd"`
	Model          string        `json:"model,omitempty"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Prompt         string        `json:"prompt"`
	StartedAt      time.Time     `json:"started_at"`
	PID            int           `json:"pid"`
	StartTimeTicks uint64        `json:"start_time_ticks,omitempty"` // supervisor start time (Linux /proc ticks, Windows FILETIME); 0 = unknown
	BootID         string        `json:"boot_id"`
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

// Job-directory file contract. Every package that touches a job dir (jobstore,
// supervisor, manager, and the test fakes) must reference these names rather
// than spelling the literals, so renaming a file is a single edit that the
// compiler propagates instead of a silent skew caught only at integration time.
const (
	MetaFile     = "meta.json"     // job metadata (atomic rewrite)
	OutFile      = "out"           // response text, appended as agy streams it
	ErrFile      = "err"           // captured agy stderr
	ExitCodeFile = "exit_code"     // completion sentinel
	CancelFile   = "cancel"        // manager -> supervisor cancel request sentinel
	ProgressFile = "progress.json" // latest stream position (atomic rewrite)
	ResultFile   = "result.json"   // terminal stream-json result payload (written once)
)

// MetaPath, OutPath, ErrPath, ExitCodePath, ProgressPath and ResultPath join a
// job directory with the corresponding file name. They are the shared spelling
// for callers that hold only the directory (the supervisor, the manager's
// status reader) rather than a store id.
func MetaPath(dir string) string     { return filepath.Join(dir, MetaFile) }
func OutPath(dir string) string      { return filepath.Join(dir, OutFile) }
func ErrPath(dir string) string      { return filepath.Join(dir, ErrFile) }
func ExitCodePath(dir string) string { return filepath.Join(dir, ExitCodeFile) }
func ProgressPath(dir string) string { return filepath.Join(dir, ProgressFile) }
func ResultPath(dir string) string   { return filepath.Join(dir, ResultFile) }

// Progress is the supervisor's running summary of a job's stream position. It
// exists so the conversation id agy reports in its init event is readable by
// the manager while the job is still running, and so a progress notification
// can name the step the run is on.
//
// It is deliberately a separate file rather than a field on Meta. The manager
// rewrites meta.json after spawning the supervisor (to record the supervisor
// PID), so a supervisor writing the same file would race that update; the
// Store mutex serializes writers within one process and cannot span the
// manager/supervisor process boundary. Splitting the file gives each one a
// single writer instead.
type Progress struct {
	ConversationID string    `json:"conversation_id,omitempty"`
	StepIndex      int       `json:"step_index"`
	StepType       string    `json:"step_type,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// WriteProgressDir atomically rewrites a job's progress file.
func WriteProgressDir(dir string, p Progress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, ProgressFile, b)
}

// ReadProgressDir reads a job's progress file. ok is false when the file is
// absent or undecodable: progress is a hint, never a correctness gate, so a
// caller treats a failed read as "nothing known yet" rather than an error.
func ReadProgressDir(dir string) (Progress, bool) {
	b, err := os.ReadFile(ProgressPath(dir))
	if err != nil {
		return Progress{}, false
	}
	var p Progress
	if err := json.Unmarshal(b, &p); err != nil {
		return Progress{}, false
	}
	return p, true
}

// WriteResultDir atomically writes a job's terminal result payload. The bytes
// are the marshalled stream-json result event; jobstore owns the file, not its
// schema, so the payload passes through opaque.
func WriteResultDir(dir string, b []byte) error {
	return writeFileAtomic(dir, ResultFile, b)
}

// ReadResultDir returns a job's terminal result payload. ok is false when no
// terminal result was recorded, which is what marks a job's captured output as
// partial.
func ReadResultDir(dir string) ([]byte, bool) {
	b, err := os.ReadFile(ResultPath(dir))
	if err != nil {
		return nil, false
	}
	return b, true
}

// CancelPath is the manager -> supervisor cancel sentinel in a job directory.
// On Windows the manager creates this file to request cancellation and the
// supervisor polls for it (Windows has no POSIX signal to deliver to an
// arbitrary process); on Linux cancel is delivered as SIGTERM instead.
func CancelPath(dir string) string { return filepath.Join(dir, CancelFile) }

// LoadDir reads and parses meta.json directly from a job directory. The
// supervisor knows only the job dir (not the store root or the id), so this
// lets it share Load's real read+unmarshal instead of re-implementing it.
func LoadDir(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(MetaPath(dir))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// WriteExitCodeDir writes the completion sentinel into a job directory. It is
// the single implementation shared by the supervisor (which knows only the dir)
// and Store.WriteExitCode (which resolves an id to its dir).
//
// The write is atomic (temp file + rename), mirroring writeMetaAtomic: a plain
// os.WriteFile truncates before writing, so a manager polling ExitCode in that
// window would read an empty file, fail to parse it, and misclassify a finished
// or cancelled job as still running or interrupted. A rename makes the sentinel
// appear in one step, never empty. 0600 matches the rest of the job-dir
// contract: the sentinel is not sensitive, but a uniform owner-only mode is
// simpler to reason about.
func WriteExitCodeDir(dir string, code int) error {
	return writeFileAtomic(dir, ExitCodeFile, []byte(strconv.Itoa(code)))
}

// writeFileAtomic writes b to dir/name via a uniquely-named temp file and a
// rename, so a reader never observes a partially written file and a crash
// mid-write leaves either the previous contents or none, never a torn mix. It
// is the single implementation behind every job-directory write (meta, exit
// code, progress, result), so none of them can drift into a non-atomic
// shortcut.
//
// 0600 throughout: these files record prompts, agy output and job metadata,
// which often embed source code, so they must not be readable by other users on
// a multi-user host. os.CreateTemp already creates 0600, so the explicit Chmod
// only guards a future change to its default.
func writeFileAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, name+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	// Flush the data before the rename so a crash cannot leave a renamed but
	// empty or short file, which a reader would parse as corrupt rather than
	// absent.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

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
// root when joined into a filesystem path. filepath.IsLocal layers on the
// platform-specific hazards a hand-rolled check misses: on Windows it rejects
// reserved device names (CON, NUL, COM1, ...) and any colon form (drive-relative
// "C:foo", NTFS alternate data stream "foo:bar"). Trailing dots and spaces are
// rejected explicitly (IsLocal allows them): Win32 path normalization strips
// them per component, so "job1." would alias job1's directory on Windows.
// Server-generated ids always pass; this guards against a malicious
// client-supplied job_id reaching the store.
func validJobID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\`) {
		return false
	}
	if !filepath.IsLocal(id) {
		return false
	}
	if strings.HasSuffix(id, ".") || strings.HasSuffix(id, " ") {
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
	if !validJobID(id) {
		return Meta{}, ErrInvalidID
	}
	return LoadDir(s.jobDir(id))
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
	return writeFileAtomic(dir, MetaFile, b)
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

// WriteExitCode writes the completion sentinel for a job id. Production writes
// the sentinel from the supervisor (which holds only the dir, via
// WriteExitCodeDir); this id-based form is the seam tests use to stage a
// terminal job. Both share WriteExitCodeDir so the on-disk contract is one
// implementation.
func (s *Store) WriteExitCode(id string, code int) error {
	if !validJobID(id) {
		return ErrInvalidID
	}
	return WriteExitCodeDir(s.jobDir(id), code)
}

// CompletedAt returns the best available estimate of when a job finished, as a
// file modification time. It prefers the exit_code sentinel (written once at a
// clean exit and never rewritten, so its mtime is a stable end timestamp) and
// falls back to the out then err file for a job recovered without a sentinel
// (e.g. a supervisor killed by a reboot), whose last write still bounds when it
// stopped producing. ok is false only when none of the three exist, in which
// case callers treat the job as still in progress for timing purposes.
func (s *Store) CompletedAt(id string) (time.Time, bool) {
	if !validJobID(id) {
		return time.Time{}, false
	}
	dir := s.jobDir(id)
	for _, name := range []string{ExitCodeFile, OutFile, ErrFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return info.ModTime(), true
		}
	}
	return time.Time{}, false
}

// ExitCode returns the recorded exit code and whether it is present.
func (s *Store) ExitCode(id string) (int, bool) {
	if !validJobID(id) {
		return 0, false
	}
	b, err := os.ReadFile(ExitCodePath(s.jobDir(id)))
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
