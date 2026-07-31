//go:build !windows

package jobstore

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestContendedIsAlwaysFalse asserts the predicate's own answer off Windows.
//
// It exists because nothing else can kill a mutation of it: flipping
// contention_notwindows.go to return true changes no observable behaviour in
// any other test, since retryWhile only consults the predicate after an error
// and every other unix test's operation succeeds first. Without this, the "the
// loop runs once" claim in contention.go rests on reading the source.
func TestContendedIsAlwaysFalse(t *testing.T) {
	dir := t.TempDir()
	// A real failure, wrapped exactly as os.Rename and os.ReadFile wrap theirs,
	// rather than a bare sentinel: the Windows twin matches on the wrapped
	// errno, so the unix side should be asked the same shape of question.
	renameErr := os.Rename(filepath.Join(dir, "missing"), filepath.Join(dir, "dst"))
	if renameErr == nil {
		t.Fatal("renaming a missing file should have failed")
	}
	if _, ok := errors.AsType[*os.LinkError](renameErr); !ok {
		t.Fatalf("os.Rename error = %T, want *os.LinkError", renameErr)
	}
	_, readErr := os.ReadFile(filepath.Join(dir, "missing"))
	if !errors.Is(readErr, fs.ErrNotExist) {
		t.Fatalf("os.ReadFile error = %v, want fs.ErrNotExist", readErr)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"rename failure", renameErr},
		{"read failure", readErr},
		{"arbitrary error", errors.New("something else")},
	} {
		if contended(tc.err) {
			t.Errorf("contended(%s) = true, want false: off Windows no error is worth retrying", tc.name)
		}
	}
}

// TestWriteFileAtomicReplacesAnOpenFile pins the kernel premise contended rests
// on off Windows: rename(2) swaps the directory entry without regard to who
// holds the old file open, so there is nothing here for a retry to wait for.
//
// It also pins the other half of what makes the replace safe. A reader that
// opened the file before the write keeps reading the version it opened, so a
// manager already mid-read of progress.json gets a consistent old snapshot
// rather than a torn mix of both. That is the property the temp-plus-rename
// exists for, and nothing else asserted it.
func TestWriteFileAtomicReplacesAnOpenFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProgressDir(dir, Progress{StepIndex: 1, StepType: "before"}); err != nil {
		t.Fatal(err)
	}
	// Opened the way the manager's poll opens it, and deliberately still open
	// across the write below.
	f, err := os.Open(ProgressPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("closing the held reader: %v", err)
		}
	}()

	if err := WriteProgressDir(dir, Progress{StepIndex: 2, StepType: "after"}); err != nil {
		t.Fatalf("replacing a file held open by a reader must succeed on unix: %v", err)
	}

	// The path now names the new file, read back through the retrying reader
	// production uses.
	got, ok := ReadProgressDir(dir)
	if !ok {
		t.Fatal("progress unreadable after the replace")
	}
	if got.StepIndex != 2 || got.StepType != "after" {
		t.Errorf("progress at the path = %+v, want the replacement (2, after)", got)
	}

	// The handle opened beforehand still names the old one, whole.
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var held Progress
	if err := json.Unmarshal(b, &held); err != nil {
		t.Fatalf("the reader that opened before the replace saw undecodable bytes (%q): %v", b, err)
	}
	if held.StepIndex != 1 || held.StepType != "before" {
		t.Errorf("reader opened before the replace saw %+v, want the snapshot it opened (1, before)", held)
	}
}
