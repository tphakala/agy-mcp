package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})
}

func TestStatusDone(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("the review"), 0o644)
	_ = m.store.WriteExitCode("j", 0)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "done" || st.Result != "the review" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusFailed(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("boom"), 0o644)
	_ = m.store.WriteExitCode("j", 5)

	st, _ := m.Status("j")
	if st.State != "failed" || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusTimedOut(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("partial"), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitTimeout)

	st, _ := m.Status("j")
	if st.State != StateFailed || !strings.Contains(st.Error, "timeout") {
		t.Fatalf("status = %+v, want failed with a timeout error", st)
	}
}

func TestTailFileReturnsRealEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "err")
	// Content longer than the requested tail; the tail must come from the END,
	// not the start (the bug: an io.LimitReader from offset 0 keeps the first N).
	content := strings.Repeat("A", 5000) + "THE-REAL-END"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(p, 20)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != content[len(content)-20:] {
		t.Fatalf("tail = %q, want the last 20 bytes", got)
	}
	if !strings.HasSuffix(got, "THE-REAL-END") {
		t.Fatalf("tail %q is not from the real end of the file", got)
	}
}

func TestTailFileShorterThanRequested(t *testing.T) {
	p := filepath.Join(t.TempDir(), "err")
	if err := os.WriteFile(p, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(p, 4096)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != "short" {
		t.Fatalf("tail = %q, want the whole short file", got)
	}
}

// TestStatusDoneButOutputUnreadable: a job that exited 0 whose out file cannot
// be read must report failed, not done with an empty result. Making out a
// directory lets os.Open succeed while the read fails, exposing the old
// readFile that collapsed every IO error into "".
func TestStatusDoneButOutputUnreadable(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", 0)

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v, want failed when the output file cannot be read", st)
	}
}

// TestStatusSpawnFail: ExitSpawnFail (127) with no stderr (a true spawn failure)
// gets a dedicated message instead of a bare "exit 127:".
func TestStatusSpawnFail(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte(""), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitSpawnFail)

	st, _ := m.Status("j")
	if st.State != StateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if !strings.Contains(st.Error, "could not exec the agy binary") {
		t.Fatalf("error = %q, want a dedicated spawn-failure message", st.Error)
	}
}

// TestStatusExit127SurfacesStderr: 127 is also a valid agy exit code, so when
// agy itself exits 127 (with stderr) the message must surface that stderr rather
// than masking it behind the spawn-failure text.
func TestStatusExit127SurfacesStderr(t *testing.T) {
	m := newTestManager(t)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("agy: internal tool not found\n"), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitSpawnFail)

	st, _ := m.Status("j")
	if st.State != StateFailed || !strings.Contains(st.Error, "internal tool not found") {
		t.Fatalf("error = %q, want it to surface agy's stderr for a real 127 exit", st.Error)
	}
}

func TestStatusInterruptedAfterReboot(t *testing.T) {
	m := newTestManager(t)
	// BootID differs from current -> the recorded PID is from a previous boot.
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("partial"), 0o644)

	st, _ := m.Status("j")
	if st.State != "done" { // no sentinel, but output present and process cannot be alive
		t.Fatalf("state = %q, want done (recovered output)", st.State)
	}
	if st.Result != "partial" {
		t.Fatalf("result = %q", st.Result)
	}
}
