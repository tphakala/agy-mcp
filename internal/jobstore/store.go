// Package jobstore persists agy-mcp job state to disk so jobs survive a
// manager restart.
package jobstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Meta describes a job. The identity fields are set at creation. PID and
// StartTimeTicks are filled in afterward by an atomic rewrite (UpdateMeta) once
// the supervisor has been spawned.
//
// ConversationID is set at creation for a continuation only, from the id the
// caller supplied or the one continue_latest resolved. A fresh run's id is never
// recorded here at all: agy names it in the init event of its stream, which
// necessarily arrives after Create, and the supervisor records it in
// progress.json. Anything that needs a fresh run's conversation id must read
// progress.json, which is why Status and conversationLive both scan it.
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
// PID), so a supervisor writing the same file would race that update, and no
// lock could arbitrate it: the two are separate processes. Splitting the file
// gives each of them a file with exactly one writer, which is what makes the
// atomic rewrite sufficient on its own.
type Progress struct {
	ConversationID string `json:"conversation_id,omitempty"`
	// StepIndex is the index of the step the run is on. No PRODUCTION code reads it
	// back off disk: the manager reports StepType, not the index. It is kept
	// because the supervisor compares it against each incoming event to decide
	// whether anything observable actually moved (see consumeStream), which is what
	// stops a repeated update for one step from atomically rewriting a
	// byte-identical file. Persisting it lets a test assert the value, which keeps
	// the ASSIGNMENT honest; the comparison itself is asserted by nothing, and
	// deleting it leaves the whole suite green, so treat that dedup as an
	// optimization a later change can silently revert.
	StepIndex int       `json:"step_index"`
	StepType  string    `json:"step_type,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
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
	b, err := readReplaced(ProgressPath(dir))
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
	return writeFileAtomicDurable(dir, ResultFile, b)
}

// ReadResultDir returns a job's terminal result payload. A missing file yields
// (nil, nil): the run never reached a terminal result, which is what marks its
// captured output as partial.
//
// Any other read failure is returned as an error rather than folded into that
// absence. The distinction is load bearing: this payload decides a job's state,
// result, error and accounting, so reporting an unreadable file as "never
// written" would silently downgrade a complete answer to a partial one and tell
// the caller their result may be truncated when it is on disk and whole.
func ReadResultDir(dir string) ([]byte, error) {
	b, err := readReplaced(ResultPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
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
	b, err := readReplaced(MetaPath(dir))
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
	return writeFileAtomicDurable(dir, ExitCodeFile, []byte(strconv.Itoa(code)))
}

// WriteCancelDir creates a job's cancel sentinel, the manager's Windows-only
// route for asking a supervisor to stop (unix delivers SIGTERM instead, so this
// has no caller there). Existence is the whole signal, so the file is empty.
//
// It goes through writeFileAtomic rather than hand-rolling a temp and a rename,
// so the job dir has one atomic writer instead of two. Note that the contention
// retry writeFileAtomic brings with it is not what motivates this: nothing ever
// opens the sentinel, since the supervisor's poll only Stats it, so this
// destination is the one that cannot be held by a reader.
func WriteCancelDir(dir string) error {
	return writeFileAtomic(dir, CancelFile, nil)
}

// writeFileAtomic writes b to dir/name via a uniquely-named temp file and a
// rename, so a reader never observes a partially written file and a crash
// mid-write leaves either the previous contents or none, never a torn mix. It
// is the single implementation behind every job-directory write (meta, exit
// code, progress, result, cancel), so none of them can drift into a non-atomic
// shortcut. out and err are the only job-dir files it does not publish; the
// supervisor appends to those as agy streams, so they are never replaced.
//
// The rename retries a destination another process holds open; see
// retryContended for why that is reachable on Windows and nowhere else.
//
// The rename itself is NOT crash-durable: tmp.Sync flushes the file's data, but
// a crash right after the rename can still lose the new directory entry. The
// write-once files whose loss would misreport a job close that gap with
// writeFileAtomicDurable; the per-step progress file and the cancel sentinel stay
// here (a directory fsync on the progress hot path would cost more than a hint
// whose loss reads as "nothing known yet", and a lost cancel is a recoverable
// control signal).
//
// 0600 throughout: these files record prompts, agy output and job metadata,
// which often embed source code, so they must not be readable by other users on
// a multi-user host. os.CreateTemp asks for 0600 (os/tempfile.go:49, go1.26.5),
// but that request still passes through the process umask, so the file can land
// tighter than 0600. The explicit Chmod sets the mode outright, which pins the
// contract against both a umask and a future change to CreateTemp's default.
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
	if err := renameReplace(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeFileAtomicDurable publishes b with writeFileAtomic and then fsyncs the
// directory, so the rename survives a crash and not just the file's data. It is
// for the low-frequency files whose loss would misreport a job afterwards: meta.json
// (its absence orphans the dir), result.json (its absence downgrades a complete
// answer to partial), and the exit_code sentinel (whose absence misreports a
// finished job's outcome).
//
// The directory fsync is BEST EFFORT. The file is already published by the time
// it runs, so a failure must not fail the write: a filesystem that cannot fsync a
// directory (some FUSE / network mounts return EINVAL) would otherwise turn a
// successful write into an error, and Create/UpdateMeta would then wipe a fresh
// job dir or tear down a running supervisor over a durability miss on a file that
// is already on disk. Losing the fsync only forfeits crash durability, so it is
// logged (a real I/O error stays visible) and swallowed; the write still
// succeeds. On mainstream local filesystems the fsync succeeds and durability is
// achieved.
func writeFileAtomicDurable(dir, name string, b []byte) error {
	if err := writeFileAtomic(dir, name, b); err != nil {
		return err
	}
	if err := fsyncDir(dir); err != nil {
		log.Printf("jobstore: directory fsync for %q failed; %s is atomic but not crash-durable: %v", dir, name, err)
	}
	return nil
}

// missingAncestors returns the ancestor directories of dir that do not exist yet,
// deepest first (dir itself, then its parent, and so on), stopping at the first
// ancestor that already exists. This is the set os.MkdirAll(dir) will have to
// create, computed BEFORE the call because MkdirAll does not report which dirs it
// made. A Stat that fails for a reason other than "does not exist" stops the walk:
// MkdirAll will surface that error, and guessing which ancestors it created past an
// unreadable one risks an fsync on a path this process never made.
func missingAncestors(dir string) []string {
	var missing []string
	for p := filepath.Clean(dir); ; {
		if _, err := os.Stat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
			break // p already exists, or we cannot tell; stop climbing
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break // reached the filesystem root
		}
		p = parent
	}
	return missing
}

// mkdirAllDurable creates dir and any missing ancestors with perm, then fsyncs the
// parent of each directory it had to create so the CREATION of the job dir (its
// entry in the parent) survives a crash, not just the files later written into it.
// Plain os.MkdirAll leaves those new directory entries only in the parent's page
// cache, so a power loss right after Create can lose the whole job dir, taking the
// just-written meta.json with it (issue #118).
//
// The parent fsyncs are best-effort, in the same spirit as writeFileAtomicDurable's
// own dir fsync: a directory-fsync-hostile filesystem logs and continues rather
// than failing job creation. On mainstream local filesystems every fsync succeeds
// and the dir creation is crash-durable.
func mkdirAllDurable(dir string, perm os.FileMode) error {
	// Capture which ancestors are missing before creating them; MkdirAll does not
	// report what it made. missing is deepest first, so slices.Backward fsyncs the
	// parents from the root down: each new dir's entry is made durable in its parent
	// before the loop reaches the dir nested inside it. So a crash leaves a durable
	// run from an existing ancestor down, never a flushed inner dir whose entry in
	// its parent is not yet on disk.
	missing := missingAncestors(dir)
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for _, d := range slices.Backward(missing) {
		parent := filepath.Dir(d)
		if err := fsyncDir(parent); err != nil {
			log.Printf("jobstore: parent-dir fsync for %q failed; creation of %q is not crash-durable: %v", parent, d, err)
		}
	}
	return nil
}

// ErrInvalidID is returned when a job id is not a safe path segment.
var ErrInvalidID = errors.New("invalid job id")

// Store is a directory-backed collection of jobs.
//
// Store holds no mutex on purpose. It once needed one to make a since-deleted
// load-modify-rewrite (SetConversationID) atomic against a concurrent
// UpdateMeta; with the conversation-cache capture gone there is no
// read-modify-write left, and every remaining writer hands in a complete Meta.
// What protects a reader is writeMetaAtomic's temp-file-and-rename, not a lock,
// and that holds across the manager/supervisor process boundary where a mutex
// could not reach anyway.
type Store struct {
	root string
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
	// mkdirAllDurable (not a bare os.MkdirAll) fsyncs each newly created ancestor's
	// parent so the CREATION of the job dir survives a crash, matching the file-write
	// durability writeMetaAtomic gives meta.json below (issue #118).
	if err := mkdirAllDurable(dir, 0o700); err != nil {
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

// UpdateMeta atomically rewrites a job's meta.json. The caller supplies a
// complete Meta, so there is nothing to serialize: writeMetaAtomic's rename is
// what stops a concurrent reader (such as the freshly spawned supervisor) from
// observing a partially written file.
func (s *Store) UpdateMeta(m Meta) error {
	if !validJobID(m.ID) {
		return ErrInvalidID
	}
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
	return writeFileAtomicDurable(dir, MetaFile, b)
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
	b, err := readReplaced(ExitCodePath(s.jobDir(id)))
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
