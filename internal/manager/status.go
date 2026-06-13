package manager

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// maxReadBytes caps how much of a job's out/err file is read into memory, so a
// runaway agy emitting huge output cannot OOM the server. Reviews are text and
// far smaller than this; anything larger is truncated.
const maxReadBytes = 32 << 20 // 32 MiB

// errTailBytes is how much of the trailing stderr a failed job reports. The tail
// (not the head) is what matters: the final lines carry the actual error.
const errTailBytes = 2000

// Job states reported by Status. These are shared with StartJob and the gate
// watchdog so the producer and consumer of a job's state cannot drift apart.
const (
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Status is the observable state of a job.
type Status struct {
	State          string // running | done | failed | cancelled
	Elapsed        time.Duration
	Result         string // present when done: captured stdout
	Error          string // present when failed: stderr tail + exit code
	ConversationID string
}

// Status derives a job's status from the on-disk store.
func (m *Manager) Status(id string) (Status, error) {
	meta, err := m.store.Load(id)
	if err != nil {
		return Status{}, err
	}
	dir, err := m.store.Dir(id)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Elapsed:        time.Since(meta.StartedAt),
		ConversationID: meta.ConversationID,
	}

	if code, ok := m.store.ExitCode(id); ok {
		return m.statusFromExitCode(dir, meta, st, code), nil
	}

	// No sentinel: decide running vs interrupted.
	if m.processAlive(meta) {
		st.State = StateRunning
		return st, nil
	}
	// The supervisor may have written the sentinel and exited between the two
	// checks above; re-read once so a job that just finished normally is not
	// misreported as interrupted.
	if code, ok := m.store.ExitCode(id); ok {
		return m.statusFromExitCode(dir, meta, st, code), nil
	}
	// Process is gone without a sentinel. If output was captured, recover it.
	out, rerr := readFile(filepath.Join(dir, "out"))
	switch {
	case rerr != nil:
		st.State = StateFailed
		st.Error = fmt.Sprintf("job process exited and its output could not be read: %v", rerr)
	case out != "":
		st.State = StateDone
		st.Result = out
		st.ConversationID = m.lazyCaptureConversationID(meta)
	default:
		st.State = StateFailed
		st.Error = "job process exited without writing a result (interrupted)"
	}
	return st, nil
}

// statusFromExitCode fills st from a recorded exit-code sentinel.
func (m *Manager) statusFromExitCode(dir string, meta jobstore.Meta, st Status, code int) Status {
	switch code {
	case 0:
		// Capture the conversation id first: a clean exit means the backend
		// conversation advanced, so even if the local out file cannot be read the
		// caller still needs the id to continue the conversation.
		st.ConversationID = m.lazyCaptureConversationID(meta)
		out, err := readFile(filepath.Join(dir, "out"))
		if err != nil {
			// The job exited cleanly but we cannot read what it produced. Report
			// that as a failure rather than a successful empty result.
			st.State = StateFailed
			st.Error = fmt.Sprintf("job completed but its output could not be read: %v", err)
			return st
		}
		st.State = StateDone
		st.Result = out
	case jobstore.ExitSIGTERM, jobstore.ExitSIGINT:
		st.State = StateCancelled
	case jobstore.ExitTimeout:
		st.State = StateFailed
		st.Error = "job exceeded its timeout and was terminated"
	case jobstore.ExitSpawnFail:
		// 127 is written both when the supervisor could not exec agy and when agy
		// itself exits 127, so name both causes rather than asserting one, and keep
		// any stderr (a true spawn failure has none; a genuine agy 127 does).
		st.State = StateFailed
		st.Error = spawnFailMessage(dir)
	default:
		st.State = StateFailed
		st.Error = errorSummary(dir, code)
	}
	return st
}

// readFile returns the file's contents (trailing newline trimmed), capped at
// maxReadBytes. A missing file yields "" with no error: a job may legitimately
// have produced no output. Any other error is returned so callers can tell an
// unreadable file (report a failure) from an empty one (a clean empty result).
func readFile(p string) (string, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// tailFile returns the last n bytes of the file at path. Unlike a LimitReader
// from offset 0 (which keeps the FIRST n bytes), it seeks to the end, so the
// tail is the real end of the stream even when the file is far larger than n.
// A missing file yields "" with no error.
func tailFile(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if size := info.Size(); size > n {
		start = size - n
	}
	buf := make([]byte, info.Size()-start)
	// A terminal job's file is normally static, but guard the TOCTOU window: if
	// the file shrank between Stat and ReadAt, ReadAt fills fewer than len(buf)
	// bytes and returns io.EOF. Slice to what was actually read so the tail is not
	// padded with NUL bytes from the unfilled allocation.
	read, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:read]), nil
}

// cleanTail returns the trailing stderr of a terminal job (the last errTailBytes,
// trailing newline trimmed and starting on a valid UTF-8 boundary), or "" when
// there is none.
func cleanTail(dir string) (string, error) {
	tail, err := tailFile(filepath.Join(dir, "err"), errTailBytes)
	if err != nil {
		return "", err
	}
	tail = strings.TrimRight(tail, "\n")
	// tailFile may have started mid-rune; advance to a valid UTF-8 boundary so a
	// multi-byte rune is not split.
	for tail != "" && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail, nil
}

func errorSummary(dir string, code int) string {
	tail, err := cleanTail(dir)
	if err != nil {
		// The stderr file exists but cannot be read; say so rather than emitting a
		// bare "exit N:" that looks like there was no error output.
		return fmt.Sprintf("exit %d: <stderr unavailable: %v>", code, err)
	}
	return strings.TrimSpace("exit " + strconv.Itoa(code) + ": " + tail)
}

// spawnFailMessage explains a 127 exit. The supervisor writes 127 both when it
// could not exec agy (the intended meaning) and when agy itself exits 127, so it
// names both causes and appends any stderr instead of masking it.
func spawnFailMessage(dir string) string {
	msg := "agy exited 127: the supervisor could not exec the agy binary (check the configured agy path), or agy itself exited 127"
	if tail, err := cleanTail(dir); err == nil && tail != "" {
		msg += "; stderr: " + tail
	}
	return msg
}
