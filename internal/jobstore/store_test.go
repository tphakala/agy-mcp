package jobstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// assertIDRejected checks that every id-taking Store method refuses id.
func assertIDRejected(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.Create(Meta{ID: id}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Create(%q) err = %v, want ErrInvalidID", id, err)
	}
	if _, err := s.Load(id); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Load(%q) err = %v, want ErrInvalidID", id, err)
	}
	if err := s.Remove(id); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Remove(%q) err = %v, want ErrInvalidID", id, err)
	}
	if _, err := s.Dir(id); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Dir(%q) err = %v, want ErrInvalidID", id, err)
	}
	if _, ok := s.ExitCode(id); ok {
		t.Errorf("ExitCode(%q) ok = true, want false", id)
	}
}

func TestRejectsUnsafeJobID(t *testing.T) {
	s := New(t.TempDir())
	// Trailing-dot/space ids are rejected on every platform because Win32 path
	// normalization strips them per component: "job1." aliases job1's directory
	// and ".. " normalizes to the parent-traversal "..".
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "job1.", "job1 ", ".. ", "..."} {
		assertIDRejected(t, s, id)
	}
}

// TestRejectsWindowsReservedJobID locks in the filepath.IsLocal layer of
// validJobID: a bare reserved device name addresses the console or a port
// device rather than a directory, so it must never be accepted as a job id.
// filepath.IsLocal is platform-dependent (these names are harmless on Unix
// filesystems and pass there), so the test runs on Windows only. Extension
// forms like "CON.txt" are deliberately not asserted: since Windows 11 they
// are no longer reserved and Go defers to RtlIsDosDeviceName_U, so the result
// is OS-version-dependent.
func TestRejectsWindowsReservedJobID(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.IsLocal rejects reserved device names and colon forms only on Windows")
	}
	s := New(t.TempDir())
	for _, id := range []string{"CON", "con", "NUL", "PRN", "AUX", "COM1", "lpt9", "foo:bar"} {
		assertIDRejected(t, s, id)
	}
}

func TestDirReturnsJobPathForValidID(t *testing.T) {
	s := New(t.TempDir())
	got, err := s.Dir("job123")
	if err != nil {
		t.Fatalf("Dir(valid) err = %v, want nil", err)
	}
	if want := s.jobDir("job123"); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
}

func TestCreateAndLoadMeta(t *testing.T) {
	s := New(t.TempDir())
	m := Meta{
		ID:        "job123",
		AgyPath:   "/usr/bin/agy",
		Args:      []string{"-p", "hi"},
		Cwd:       "/work",
		StartedAt: time.Unix(1000, 0).UTC(),
		PID:       4242,
		BootID:    "boot-abc",
	}
	dir, err := s.Create(m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
	got, err := s.Load("job123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PID != 4242 || got.AgyPath != "/usr/bin/agy" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Cwd != "/work" || got.BootID != "boot-abc" {
		t.Fatalf("round-trip cwd/bootid mismatch: %+v", got)
	}
	if !slices.Equal(got.Args, m.Args) {
		t.Fatalf("round-trip args = %v, want %v", got.Args, m.Args)
	}
	if !got.StartedAt.Equal(m.StartedAt) {
		t.Fatalf("round-trip startedAt = %v, want %v", got.StartedAt, m.StartedAt)
	}
}

func TestCreateCleansUpDirOnWriteFailure(t *testing.T) {
	s := New(t.TempDir())
	dir := s.jobDir("j")
	// Make meta.json a directory so Create's WriteFile fails after MkdirAll has
	// already created the job dir. Without cleanup the empty dir would be orphaned:
	// GarbageCollect skips it forever because Load (no meta.json) errors.
	if err := os.MkdirAll(filepath.Join(dir, "meta.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Meta{ID: "j"}); err == nil {
		t.Fatal("Create should fail when meta.json cannot be written")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("job dir should be removed after a failed Create; stat err = %v", err)
	}
}

func TestStartTimeTicksRoundTripAndBackCompat(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j", PID: 4242, StartTimeTicks: 987654}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Load("j")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartTimeTicks != 987654 {
		t.Fatalf("round-trip StartTimeTicks = %d, want 987654", got.StartTimeTicks)
	}

	// A meta.json written by an older binary has no start_time_ticks field; it must
	// load as 0 ("unknown"), which disables the start-time liveness check rather
	// than misbehaving.
	old := New(t.TempDir())
	dir := old.jobDir("legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"id":"legacy","agy_path":"/usr/bin/agy","args":["-p","hi"],"cwd":"/work","prompt":"hi","started_at":"2026-01-01T00:00:00Z","pid":7,"boot_id":"b"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	lm, err := old.Load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if lm.StartTimeTicks != 0 {
		t.Fatalf("legacy meta StartTimeTicks = %d, want 0", lm.StartTimeTicks)
	}
}

func TestUpdateMetaRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMeta(Meta{ID: "j", PID: 99, AgyPath: "/x"}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	got, err := s.Load("j")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 99 || got.AgyPath != "/x" {
		t.Fatalf("UpdateMeta round-trip = %+v", got)
	}
}

func TestExitCodeSentinel(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ExitCode("j"); ok {
		t.Fatal("exit code should be absent before completion")
	}
	if err := s.WriteExitCode("j", 0); err != nil {
		t.Fatal(err)
	}
	code, ok := s.ExitCode("j")
	if !ok || code != 0 {
		t.Fatalf("ExitCode = %d,%v", code, ok)
	}
}

// TestWriteCancelDirCreatesAnEmptySentinelWhereTheSupervisorLooks pins the
// cancel sentinel's contract, which is unusual in that the file's existence is
// the entire message: the manager writes it (Windows only, where there is no
// signal to send) and the supervisor's poll does nothing but Stat CancelPath.
//
// Both properties asserted here are load-bearing. Writing it where CancelPath
// reads is what makes the two processes agree, and a second Cancel of the same
// job has to stay a success, because the manager's Cancel is reachable more than
// once for one job and a caller reads an error as "the cancel did not happen".
func TestWriteCancelDirCreatesAnEmptySentinelWhereTheSupervisorLooks(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.Create(Meta{ID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CancelPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat before the write = %v, want the sentinel absent", err)
	}
	if err := WriteCancelDir(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(CancelPath(dir))
	if err != nil {
		t.Fatalf("sentinel not written where CancelPath reads: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("sentinel content = %q, want empty: existence is the whole signal", b)
	}
	// Replacing an existing sentinel must not fail, so a repeated cancel is not
	// reported as an error.
	if err := WriteCancelDir(dir); err != nil {
		t.Errorf("second WriteCancelDir = %v, want nil", err)
	}
	// The temp file the atomic write goes through must not survive it, or the
	// job dir accumulates one per cancel.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q after the atomic write", e.Name())
		}
	}
}

// TestDirContractSharedByStoreAndHelpers pins the centralized job-dir file
// contract: the dir-based helpers (LoadDir, WriteExitCodeDir, the *Path
// helpers) operate on the exact files the id-based Store methods read and
// write, so the supervisor (which holds only the dir) and the manager (which
// goes through Store by id) cannot drift apart.
func TestDirContractSharedByStoreAndHelpers(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.Create(Meta{ID: "j", AgyPath: "/bin/agy", Prompt: "hi", StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	// Create wrote meta.json where LoadDir reads it.
	if got, err := LoadDir(dir); err != nil || got.ID != "j" || got.Prompt != "hi" {
		t.Fatalf("LoadDir(dir) = %+v, %v; want the meta Store.Create wrote", got, err)
	}
	if MetaPath(dir) != filepath.Join(dir, "meta.json") {
		t.Fatalf("MetaPath(dir) = %q, want it under the job dir", MetaPath(dir))
	}

	// The supervisor's WriteExitCodeDir is observed by the manager's Store.ExitCode.
	if err := WriteExitCodeDir(dir, 42); err != nil {
		t.Fatal(err)
	}
	if code, ok := s.ExitCode("j"); !ok || code != 42 {
		t.Fatalf("ExitCode after WriteExitCodeDir = %d, %v; want 42, true", code, ok)
	}
	// And the reverse: Store.WriteExitCode (id path) is read back at ExitCodePath (dir path).
	if err := s.WriteExitCode("j", 7); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(ExitCodePath(dir)); err != nil || string(b) != "7" {
		t.Fatalf("ExitCodePath content = %q, %v; want \"7\"", b, err)
	}
}

func TestListAndRemove(t *testing.T) {
	s := New(t.TempDir())
	_, _ = s.Create(Meta{ID: "a", StartedAt: time.Unix(0, 0)})
	_, _ = s.Create(Meta{ID: "b", StartedAt: time.Now()})
	ids, err := s.List()
	if err != nil || len(ids) != 2 {
		t.Fatalf("List = %v, %v", ids, err)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ids, _ = s.List()
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("after Remove, List = %v", ids)
	}
}

// TestMissingAncestorsStopsAtFirstExisting pins missingAncestors, the pre-scan
// mkdirAllDurable relies on to learn which parent dirs need an fsync after
// MkdirAll (MkdirAll does not report what it created). The walk must list every
// not-yet-existing ancestor, deepest first, and stop at the first one that
// already exists so an fsync never runs against a dir Create did not make.
func TestMissingAncestorsStopsAtFirstExisting(t *testing.T) {
	base := t.TempDir() // already exists
	target := filepath.Join(base, "a", "b", "c")
	got := missingAncestors(target)
	want := []string{
		target,
		filepath.Join(base, "a", "b"),
		filepath.Join(base, "a"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("missingAncestors(%q) = %v, want %v", target, got, want)
	}
	// An already-existing directory has no missing ancestors, so nothing is synced.
	if got := missingAncestors(base); len(got) != 0 {
		t.Fatalf("missingAncestors(existing) = %v, want empty", got)
	}
}

// TestCreateBuildsAndLoadsThroughMissingStateRoot exercises Create's durable-dir
// path when several ancestors are missing: a state root nested two levels deep,
// so mkdirAllDurable must create <root>, <root>/jobs and the job dir, then run its
// parent-fsync loop over more than one new dir. The parent fsyncs themselves are
// not observable from a test (fsync has no user-visible effect absent a crash), so
// this asserts only the functional contract the durable path must preserve: a
// loadable job dir with meta.json. missingAncestors' own test pins which parents
// get synced.
func TestCreateBuildsAndLoadsThroughMissingStateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "nested") // does not exist yet
	s := New(root)
	dir, err := s.Create(Meta{ID: "job1", BootID: "b"})
	if err != nil {
		t.Fatalf("Create through missing state root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, MetaFile)); err != nil {
		t.Fatalf("meta.json missing after Create: %v", err)
	}
	if _, err := s.Load("job1"); err != nil {
		t.Fatalf("Load after Create: %v", err)
	}
}
