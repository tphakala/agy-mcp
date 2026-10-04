package manager

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// TestNormalizeCwdCollapsesEquivalentSpellings is the core regression guard for
// issue #24: a trailing slash, a relative path, and a symlinked alias of one
// directory must all canonicalize to the same absolute, symlink-resolved path,
// so they hit the same agy conversation-cache entry and hand the supervisor the
// same cmd.Dir. They no longer have to agree on a gate key; fresh runs stopped
// serializing by directory when keyFor narrowed to the conversation id.
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

// TestRootRelative pins which spellings never stand in for cwd. They are
// Windows-only shapes; on POSIX a backslash or a "C:" prefix is part of a file
// name and an absolute path is not root-relative.
func TestRootRelative(t *testing.T) {
	for _, tc := range []struct {
		d       string
		windows bool // the answer on Windows
		posix   bool // the answer elsewhere
	}{
		{d: `\`, windows: true},
		{d: `\a\..`, windows: true},
		{d: `C:repo`, windows: true},
		{d: `C:.`, windows: true},
		{d: `C:\x`},
		{d: `\\server\share\x`},
		{d: `rel`},
		{d: `.`},
		{d: `/x`},
	} {
		want := tc.posix
		if runtime.GOOS == "windows" {
			want = tc.windows
		}
		if got := rootRelative(tc.d); got != want {
			t.Errorf("rootRelative(%q) = %v, want %v on %s", tc.d, got, want, runtime.GOOS)
		}
	}
}

// TestJoinAsWrittenAtRoot: a cwd that already ends in a separator (a filesystem
// root) must not gain a second one, which would turn "/dir" into "//dir".
func TestJoinAsWrittenAtRoot(t *testing.T) {
	sep := string(filepath.Separator)
	tmp := t.TempDir()
	root := filepath.VolumeName(tmp) + sep
	if got, want := joinAsWritten(root, "dir"), root+"dir"; got != want {
		t.Errorf("joinAsWritten(%q, dir) = %q, want %q", root, got, want)
	}
	if got, want := joinAsWritten(tmp, "dir"), tmp+sep+"dir"; got != want {
		t.Errorf("joinAsWritten(%q, dir) = %q, want %q", tmp, got, want)
	}
}

// TestDirsIncludeMissingCwdNeverMatches: a cwd that does not exist never counts
// as named by a caller dir, so the implicit --add-dir stays. The entry's own stat
// already fails here, so this pins the outcome, not the lazy cwd stat.
func TestDirsIncludeMissingCwdNeverMatches(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if dirsInclude([]string{"."}, missing) {
		t.Fatal("dirsInclude reported a match for a cwd that does not exist")
	}
}
