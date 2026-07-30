package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSignalWaitReady pins the readiness handshake's production half. The
// POSIX interrupt tests exercise it end to end, but they do so in a child
// process built by buildBinary, so nothing in this package's own coverage
// touches it and every claim its doc comment makes goes unenforced. These cases
// are the claims.
//
// No t.Parallel anywhere here: every case drives the same process-global
// environment variable through t.Setenv, which forbids it.
func TestSignalWaitReady(t *testing.T) {
	t.Run("creates the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ready")
		t.Setenv(waitReadyFileEnv, path)
		signalWaitReady()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("readiness file not created: %v", err)
		}
	})

	// Two things make this assertion mean something. The code is aimed at the
	// directory and shown to write there before the aim is taken away, so the
	// absence afterwards is a fact about a path the function knows; and the
	// directory is also the process's working directory, so a guard that stopped
	// returning early and fell back to a bare filename would land here too.
	// Without both, the subtest holds for every possible implementation, including
	// one that writes on every call. It still cannot see a fallback to some
	// unrelated absolute path, which no assertion short of scanning the filesystem
	// could.
	t.Run("unset writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		path := filepath.Join(dir, "ready")
		t.Setenv(waitReadyFileEnv, path)
		signalWaitReady()
		if err := os.Remove(path); err != nil {
			t.Fatalf("the readiness file was not written where the test aimed it: %v", err)
		}
		t.Setenv(waitReadyFileEnv, "")
		signalWaitReady()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("wrote %d entries with the variable unset, want none", len(entries))
		}
	})

	// The destructive case the O_EXCL open exists to refuse. A user who exports
	// the variable once and forgets it aims every later wait-job and hook-wait at
	// that path, and an ordinary create would truncate whatever is there.
	t.Run("an existing file is left intact", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "precious")
		const content = "do not truncate me"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(waitReadyFileEnv, path)
		signalWaitReady()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Fatalf("existing file = %q, want it untouched (%q)", got, content)
		}
	})

	// The swallowed-error promise: a path that cannot be created must still return
	// normally, because this call sits between the signal handler and the wait it
	// announces and may never derail either. It is a smoke case by nature, since
	// the only observable of a swallowed error is that nothing happens: it proves
	// the call returns and creates no file, not how it failed.
	t.Run("a path that cannot be created is swallowed", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(waitReadyFileEnv, filepath.Join(dir, "no-such-dir", "ready"))
		signalWaitReady()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("created %d entries under a missing parent directory, want none", len(entries))
		}
	})
}
