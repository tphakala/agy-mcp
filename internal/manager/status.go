package manager

import (
	"io"
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
	dir := m.store.Dir(id)
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
	out := readFile(filepath.Join(dir, "out"))
	if out != "" {
		st.State = StateDone
		st.Result = out
		st.ConversationID = m.lazyCaptureConversationID(meta)
	} else {
		st.State = StateFailed
		st.Error = "job process exited without writing a result (interrupted)"
	}
	return st, nil
}

// statusFromExitCode fills st from a recorded exit-code sentinel.
func (m *Manager) statusFromExitCode(dir string, meta jobstore.Meta, st Status, code int) Status {
	switch code {
	case 0:
		st.State = StateDone
		st.Result = readFile(filepath.Join(dir, "out"))
		st.ConversationID = m.lazyCaptureConversationID(meta)
	case jobstore.ExitSIGTERM, jobstore.ExitSIGINT:
		st.State = StateCancelled
	case jobstore.ExitTimeout:
		st.State = StateFailed
		st.Error = "job exceeded its timeout and was terminated"
	default:
		st.State = StateFailed
		st.Error = errorSummary(dir, code)
	}
	return st
}

func readFile(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

func errorSummary(dir string, code int) string {
	tail := readFile(filepath.Join(dir, "err"))
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
		// Advance to a valid UTF-8 boundary so a multi-byte rune is not split.
		for tail != "" && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	return strings.TrimSpace("exit " + strconv.Itoa(code) + ": " + tail)
}
