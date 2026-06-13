//go:build unix

package jobstore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// clearUmask sets the process umask to 0 for the duration of the test and
// restores it afterward, so a permission assertion isolates the explicit mode
// the code requests from the ambient umask. Without it a regression that drops
// the explicit mode (e.g. back to os.Create's 0666) could still pass under a
// tight umask that happens to mask the extra bits away. Tests run sequentially
// within a package, so toggling the process-global umask here is safe.
func clearUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestCreateAndWriteExitCodeUsePrivatePerms(t *testing.T) {
	// Job dirs and files hold prompts and full agy output, which often embed
	// source code. On a multi-user host with a traversable home, 0755/0644 would
	// let other users read them; 0700 dirs and 0600 files keep them private.
	clearUmask(t)
	s := New(t.TempDir())
	dir, err := s.Create(Meta{ID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if di, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("job dir perm = %o, want 700", got)
	}
	if mi, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	} else if got := mi.Mode().Perm(); got != 0o600 {
		t.Errorf("meta.json perm = %o, want 600", got)
	}
	if err := s.WriteExitCode("j", 0); err != nil {
		t.Fatal(err)
	}
	if ei, err := os.Stat(filepath.Join(dir, "exit_code")); err != nil {
		t.Fatal(err)
	} else if got := ei.Mode().Perm(); got != 0o600 {
		t.Errorf("exit_code perm = %o, want 600", got)
	}
}

func TestUpdateMetaPreservesPrivatePerms(t *testing.T) {
	// A rewrite via the temp+rename path must not loosen meta.json back to 0644.
	clearUmask(t)
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMeta(Meta{ID: "j", PID: 7}); err != nil {
		t.Fatal(err)
	}
	mi, err := os.Stat(filepath.Join(s.jobDir("j"), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := mi.Mode().Perm(); got != 0o600 {
		t.Errorf("meta.json perm after UpdateMeta = %o, want 600", got)
	}
}
