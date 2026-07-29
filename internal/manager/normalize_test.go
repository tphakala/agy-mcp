package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
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
// by an older binary may have a raw, un-normalized meta.Cwd, and reqFromMeta
// must canonicalize it. The cwd no longer feeds the gate key (only a
// conversation does), but it still has to be normalized so a restored job's cwd
// matches the spelling StartJob persists.
func TestReqFromMetaNormalizesLegacyCwd(t *testing.T) {
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	req := reqFromMeta(jobstore.Meta{Cwd: dir + "/"})
	if req.Cwd != canonical {
		t.Errorf("restored cwd = %q, want %q (legacy cwd not normalized)", req.Cwd, canonical)
	}
	// A restored fresh run keys on nothing, so it blocks no new run.
	if got := keyFor(req); got != "" {
		t.Errorf("restored gate key = %q, want empty for a fresh run", got)
	}
}

// A restored job that was continuing a conversation still keys on it, which is
// the serialization that remains.
func TestReqFromMetaKeysOnConversation(t *testing.T) {
	req := reqFromMeta(jobstore.Meta{Cwd: t.TempDir(), ConversationID: "cid-9"})
	if got, want := keyFor(req), "conv:cid-9"; got != want {
		t.Errorf("restored gate key = %q, want %q", got, want)
	}
}

// TestNormalizeCwdPreservesEmpty locks the empty-cwd guard: filepath.Abs("")
// resolves to the process working directory, which would turn a legacy job's
// empty meta.Cwd into a bogus, unrelated gate key. An empty cwd must stay empty.
func TestNormalizeCwdPreservesEmpty(t *testing.T) {
	got, err := normalizeCwd("")
	if err != nil {
		t.Fatalf("normalizeCwd(empty): %v", err)
	}
	if got != "" {
		t.Errorf("normalizeCwd(empty) = %q, want empty", got)
	}
}

// TestReqFromMetaPreservesEmptyCwd verifies a legacy job persisted with no cwd
// restores under the no-key behavior, not under the manager's working directory.
func TestReqFromMetaPreservesEmptyCwd(t *testing.T) {
	if k := keyFor(reqFromMeta(jobstore.Meta{Cwd: ""})); k != "" {
		t.Errorf("restored gate key for empty cwd = %q, want empty", k)
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
