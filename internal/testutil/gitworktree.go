package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// SkipIfInGitWorkTree skips the test when dir, or any directory above it, holds
// a .git entry. An isolated run is refused under a git work tree, so a test that
// starts one under t.TempDir() cannot run when the temp directory sits inside a
// repository.
func SkipIfInGitWorkTree(t *testing.T, dir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	for p := resolved; ; {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			t.Skipf("%s is inside a git work tree (%s has .git); an isolated run is refused there", dir, p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return
		}
		p = parent
	}
}
