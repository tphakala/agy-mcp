package manager

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
		if code == 0 {
			st.State = "done"
			st.Result = readFile(filepath.Join(dir, "out"))
		} else if code == 143 || code == 130 {
			st.State = "cancelled"
		} else {
			st.State = "failed"
			st.Error = errorSummary(dir, code)
		}
		return st, nil
	}

	// No sentinel: decide running vs interrupted.
	if m.processAlive(meta) {
		st.State = "running"
		return st, nil
	}
	// Process is gone without a sentinel. If output was captured, recover it.
	out := readFile(filepath.Join(dir, "out"))
	if out != "" {
		st.State = "done"
		st.Result = out
	} else {
		st.State = "failed"
		st.Error = "job process exited without writing a result (interrupted)"
	}
	return st, nil
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

func errorSummary(dir string, code int) string {
	tail := readFile(filepath.Join(dir, "err"))
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	return strings.TrimSpace("exit " + itoa(code) + ": " + tail)
}

func itoa(i int) string { return strconv.Itoa(i) }
