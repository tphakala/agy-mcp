package jobstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestContendedClassifiesTheWrappedErrnos is the predicate's own table.
//
// Without it nothing distinguishes contended from "retry everything": both race
// tests below produce a sharing violation, so replacing this function's body
// with `return true` leaves them green. The rows that carry their weight are
// the negative ones, and fs.ErrNotExist most of all: readReplaced's whole
// design rests on absence being answered immediately rather than retried for
// 213ms on every poll of a running job.
func TestContendedClassifiesTheWrappedErrnos(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			// The shape os.Rename returns when the destination is held open.
			name: "sharing violation wrapped by os.Rename",
			err:  &os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_SHARING_VIOLATION},
			want: true,
		},
		{
			// The shape os.ReadFile returns; the arm the race tests never reach.
			name: "access denied wrapped by os.ReadFile",
			err:  &fs.PathError{Op: "open", Path: "p", Err: windows.ERROR_ACCESS_DENIED},
			want: true,
		},
		{
			name: "sharing violation unwrapped",
			err:  windows.ERROR_SHARING_VIOLATION,
			want: true,
		},
		{
			// Deliberately NOT retried, unlike robustio's predicate. See readReplaced.
			name: "file absent",
			err:  &fs.PathError{Op: "open", Path: "p", Err: windows.ERROR_FILE_NOT_FOUND},
			want: false,
		},
		{
			name: "fs.ErrNotExist",
			err:  fs.ErrNotExist,
			want: false,
		},
		{
			name: "arbitrary error",
			err:  errors.New("something else"),
			want: false,
		},
	} {
		if got := contended(tc.err); got != tc.want {
			t.Errorf("contended(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRetryContendedOutlastsAnOpenReader is the writer half of the race this
// package retries for, exercised against the real OS rather than a sentinel.
//
// It asserts two things at once, and both matter:
//
//   - The first os.Rename over a destination another handle holds open really
//     does fail here. os.Open asks for FILE_SHARE_READ|FILE_SHARE_WRITE and not
//     FILE_SHARE_DELETE, so MoveFileEx cannot get the DELETE access it needs.
//     Reaching a second attempt is what proves the failure happened; if this OS
//     ever stops behaving that way the test says so instead of quietly passing.
//   - contended classifies that real errno as retryable, since the loop only
//     goes round again when it returns true.
//
// The handle is released from inside the operation, immediately after the
// attempt it is meant to defeat. That ordering is the point: no timing margin,
// and the retry has something to succeed at the moment it runs. time.Sleep is
// real rather than a no-op so the remaining attempts span the same wall time
// production gives them; with a no-op the test would be strictly LESS tolerant
// of a scanner on a CI runner than the code it guards.
func TestRetryContendedOutlastsAnOpenReader(t *testing.T) {
	dir := t.TempDir()
	dst := ProgressPath(dir)
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := dst + ".tmp"
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Held open the way the manager's status poll holds progress.json.
	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Safety net for the path where the operation below never runs; the
	// operation closes it on the first attempt, and a double close is a
	// harmless already-closed error.
	defer func() { _ = held.Close() }()

	attempts := 0
	err = retryWhile(
		func() error {
			attempts++
			rerr := os.Rename(src, dst)
			if attempts == 1 {
				// Release only after the contended attempt has already failed,
				// so the retry is what succeeds rather than a lucky interleaving.
				_ = held.Close()
			}
			return rerr
		},
		contended,
		time.Sleep,
	)
	if err != nil {
		t.Fatalf("rename over a reader-held destination failed after %d attempts: %v", attempts, err)
	}
	// Not equality: a scanner can lose attempt 2 as well, and the retry would
	// still have done its job on attempt 3. What must hold is that attempt 1
	// failed, which is the whole premise.
	if attempts < 2 {
		t.Errorf("attempts = %d, want at least 2: the first rename must fail against the open handle "+
			"and a retry must be what replaces the file", attempts)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Errorf("destination = %q, want the replacement %q", b, "new")
	}
}

// TestRetryContendedOutlastsAnExclusiveHolder is the reader half of the same
// race. A concurrent replace can leave a job-dir file briefly unopenable, which
// is why readReplaced retries and why the standard library's robustio.ReadFile
// exists at all.
//
// The stimulus is a handle opened with no sharing at all, which is the
// deterministic way to make any other open fail with ERROR_SHARING_VIOLATION.
// It stands in for the replace window rather than reproducing it, because the
// replace window cannot be held open on demand; what it proves is the part that
// matters and that no other test reaches, namely that a real read-side errno is
// classified as retryable and that the retry then succeeds.
func TestRetryContendedOutlastsAnExclusiveHolder(t *testing.T) {
	dir := t.TempDir()
	path := ProgressPath(dir)
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The sync.Once inside release is load-bearing, not tidiness: Windows
	// recycles handle values, so closing twice could close something unrelated
	// that os.ReadFile opened in between. The deferred call covers the path
	// where the operation below never runs.
	release := openExclusive(t, path)
	defer release()

	var b []byte
	attempts := 0
	err := retryWhile(
		func() error {
			attempts++
			var rerr error
			b, rerr = os.ReadFile(path)
			if attempts == 1 {
				// Release only after the contended attempt has already failed.
				release()
			}
			return rerr
		},
		contended,
		time.Sleep,
	)
	if err != nil {
		t.Fatalf("reading a file held exclusively failed after %d attempts: %v", attempts, err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want at least 2: the first read must fail against the exclusive handle", attempts)
	}
	if string(b) != "payload" {
		t.Errorf("content = %q, want %q", b, "payload")
	}
}

// openExclusive opens path with no sharing at all, so any other open asking for
// read, write or delete access fails with ERROR_SHARING_VIOLATION until the
// returned func runs. It is the deterministic way to hold a job-dir file against
// a reader. An attribute-only query such as os.Stat is exempt from the
// share-mode check and still succeeds; nothing here depends on it failing.
func openExclusive(t *testing.T, path string) func() {
	t.Helper()
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p16, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("opening %s exclusively: %v", path, err)
	}
	var once sync.Once
	return func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
}

// TestJobDirReadersSpendTheBudgetOnAContendedFile pins the wiring of all four
// job-dir readers to readReplaced.
//
// It exists because nothing else pins it on any platform. Off Windows contended
// is constant-false, so readReplaced and os.ReadFile are indistinguishable by
// construction; on Windows the handshake tests above drive retryWhile directly
// rather than going through these callers. Reverting all four call sites to a
// bare os.ReadFile therefore left the entire suite green, which is how a later
// edit would undo this silently.
//
// The stimulus is held for the whole read rather than released partway, so the
// assertion needs no timing margin: a reader wired to the retry exhausts the
// budget and fails after ~213ms, while one wired straight to os.ReadFile fails
// in microseconds. The threshold sits far below the budget and far above a
// single failed open, so neither a fast nor a loaded runner can cross it.
func TestJobDirReadersSpendTheBudgetOnAContendedFile(t *testing.T) {
	const id = "j"
	for _, tc := range []struct {
		name string
		file string
		// stage puts the file in place; nil when Store.Create already wrote it.
		stage func(dir string) error
		// read reports whether the reader returned an answer. Every one of these
		// reports failure differently, which is itself the reason to drive them
		// through their real signatures rather than readReplaced directly.
		read func(s *Store, dir string) bool
	}{
		{
			name:  "ReadProgressDir",
			file:  ProgressFile,
			stage: func(dir string) error { return WriteProgressDir(dir, Progress{StepIndex: 1}) },
			read:  func(_ *Store, dir string) bool { _, ok := ReadProgressDir(dir); return ok },
		},
		{
			name:  "ReadResultDir",
			file:  ResultFile,
			stage: func(dir string) error { return WriteResultDir(dir, []byte(`{"status":"ok"}`)) },
			read:  func(_ *Store, dir string) bool { b, err := ReadResultDir(dir); return err == nil && b != nil },
		},
		{
			name: "LoadDir",
			file: MetaFile,
			read: func(_ *Store, dir string) bool { _, err := LoadDir(dir); return err == nil },
		},
		{
			name:  "Store.ExitCode",
			file:  ExitCodeFile,
			stage: func(dir string) error { return WriteExitCodeDir(dir, 0) },
			read:  func(s *Store, _ string) bool { _, ok := s.ExitCode(id); return ok },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t.TempDir())
			dir, err := s.Create(Meta{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if tc.stage != nil {
				if err := tc.stage(dir); err != nil {
					t.Fatal(err)
				}
			}

			release := openExclusive(t, filepath.Join(dir, tc.file))
			defer release()

			start := time.Now()
			ok := tc.read(s, dir)
			elapsed := time.Since(start)

			if ok {
				t.Fatalf("%s answered while %s was held exclusively; the stimulus did not take, "+
					"so this test proves nothing", tc.name, tc.file)
			}
			if floor := 100 * time.Millisecond; elapsed < floor {
				t.Errorf("%s gave up on a contended %s after %v, want at least %v: it is reading through "+
					"os.ReadFile directly rather than readReplaced", tc.name, tc.file, elapsed, floor)
			}
		})
	}
}

// TestWriteFileAtomicSurvivesAnOpenReader is the writer race one layer up,
// through the function production actually calls: the supervisor republishing
// progress.json while the manager holds it open must not lose the write.
//
// It cannot use the handshake the tests above use, because writeFileAtomic owns
// its own rename and exposes no seam, which is exactly why the layer above is
// worth a separate test. The release is therefore timed, at a fraction of the
// 213ms budget.
//
// The channel check afterwards is a NECESSARY condition, not proof: it catches
// the write finishing before the handle was released, which would mean the test
// never met contention at all. It cannot prove the converse, since a runner
// slow enough to spend 20ms inside CreateTemp and Sync would release the handle
// before the first rename and still pass. The deterministic proof that the
// retry works lives in the two handshake tests above; this one is the
// integration check that writeFileAtomic is wired to it.
func TestWriteFileAtomicSurvivesAnOpenReader(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProgressDir(dir, Progress{StepIndex: 1, StepType: "before"}); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(ProgressPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(20 * time.Millisecond)
		_ = held.Close()
	}()
	// Joins the goroutine before t.TempDir's own cleanup removes the directory,
	// since cleanups run last-registered-first.
	t.Cleanup(func() { <-released })

	if err := WriteProgressDir(dir, Progress{StepIndex: 2, StepType: "after"}); err != nil {
		t.Fatalf("progress rewrite lost to a reader holding the file open: %v", err)
	}
	select {
	case <-released:
	default:
		t.Error("the write completed while the reader still held the file, so it never met contention; " +
			"this test proved nothing about the retry")
	}
	got, ok := ReadProgressDir(dir)
	if !ok {
		t.Fatal("progress unreadable after the replace")
	}
	if got.StepIndex != 2 || got.StepType != "after" {
		t.Errorf("progress = %+v, want the replacement (2, after)", got)
	}
}
