package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// TestNormalizeCwdCollapsesEquivalentSpellings is the core regression guard for
// issue #24: a trailing slash, a relative path, and a symlinked alias of one
// directory must all canonicalize to the same absolute, symlink-resolved path,
// so they produce one gate key (same-dir fresh runs serialize) and hit the same
// agy conversation-cache entry.
func TestNormalizeCwdCollapsesEquivalentSpellings(t *testing.T) {
	realDir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realDir, err)
	}

	t.Run("trailing slash", func(t *testing.T) {
		got, err := normalizeCwd(realDir + string(filepath.Separator))
		if err != nil {
			t.Fatalf("normalizeCwd: %v", err)
		}
		if got != canonical {
			t.Errorf("got %q, want %q", got, canonical)
		}
	})

	t.Run("symlinked alias", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		got, err := normalizeCwd(link)
		if err != nil {
			t.Fatalf("normalizeCwd: %v", err)
		}
		if got != canonical {
			t.Errorf("symlink resolved to %q, want %q", got, canonical)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		t.Chdir(filepath.Dir(realDir))
		got, err := normalizeCwd(filepath.Base(realDir))
		if err != nil {
			t.Fatalf("normalizeCwd: %v", err)
		}
		if got != canonical {
			t.Errorf("relative path resolved to %q, want %q", got, canonical)
		}
	})
}

// TestReqFromMetaNormalizesLegacyCwd guards the upgrade window: a job persisted
// by an older binary may have a raw, un-normalized meta.Cwd. RestoreGate must
// fold it onto the same normalized gate key a new same-dir run computes, or the
// two would not serialize and could issue concurrent O_TRUNC writes to the same
// agy cache entry. Regression test for issue #24.
func TestReqFromMetaNormalizesLegacyCwd(t *testing.T) {
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	req := reqFromMeta(jobstore.Meta{Cwd: dir + "/"})
	want := "cwd:" + canonical
	if got := keyFor(req); got != want {
		t.Errorf("restored gate key = %q, want %q (legacy cwd not normalized)", got, want)
	}
}

// TestNormalizeCwdKeepsAbsoluteFormWhenUnresolvable verifies the best-effort
// symlink step does not fail the run for a path that does not exist yet: it
// falls back to the cleaned absolute form (agy itself will fail on a bad cwd).
func TestNormalizeCwdKeepsAbsoluteFormWhenUnresolvable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "sub")
	got, err := normalizeCwd(missing)
	if err != nil {
		t.Fatalf("normalizeCwd should not fail on a missing path: %v", err)
	}
	if got != filepath.Clean(missing) {
		t.Errorf("got %q, want cleaned absolute %q", got, filepath.Clean(missing))
	}
}
