//go:build unix

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func TestSupervisorWritesPrivatePerms(t *testing.T) {
	// out/err capture agy output (which often embeds source code) and exit_code is
	// the supervisor's own write; all must be owner-only on a multi-user host.
	// Clear the umask so the assertion checks the explicit mode the code sets, not
	// whatever the ambient umask happens to mask away.
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", Stderr: "y", Exit: 0})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, Args: []string{"-p", "x"}, StartedAt: time.Now()})
	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"out", "err", "exit_code"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s perm = %o, want 600", name, got)
		}
	}
}
