package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// SkipIfInGitWorkTree must skip a test whose directory has a .git entry above
// it, and must let a test in a clean tree run. Not build-tagged: the helper
// needs no shell, and internal/manager/isolated_test.go, which calls it, also
// runs on Windows.
func TestSkipIfInGitWorkTree(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	reached := false
	t.Run("inside", func(st *testing.T) {
		SkipIfInGitWorkTree(st, inner)
		reached = true
	})
	if reached {
		t.Fatalf("SkipIfInGitWorkTree(%s) did not skip with .git two levels up", inner)
	}

	clean := t.TempDir()
	if dotGitAbove(t, clean) {
		t.Skipf("%s is inside a git work tree, so the clean case cannot be checked here", clean)
	}
	reached = false
	t.Run("outside", func(st *testing.T) {
		SkipIfInGitWorkTree(st, clean)
		reached = true
	})
	if !reached {
		t.Fatalf("SkipIfInGitWorkTree(%s) skipped with no .git above it", clean)
	}
}

// dotGitAbove reports whether dir or any directory above it has a .git entry.
// It decides on its own whether the clean case applies, so a helper that always
// skips fails the test instead of turning it into a skip.
func dotGitAbove(t *testing.T, dir string) bool {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}
