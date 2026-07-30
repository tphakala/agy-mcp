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

	t.Run("unset writes nothing", func(t *testing.T) {
		dir := t.TempDir()
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

	// The swallowed-error promise: a path that cannot be written must return
	// normally, because this call sits between the signal handler and the wait it
	// announces and may never derail either.
	t.Run("an unwritable path is swallowed", func(t *testing.T) {
		t.Setenv(waitReadyFileEnv, filepath.Join(t.TempDir(), "no-such-dir", "ready"))
		signalWaitReady()
	})
}
