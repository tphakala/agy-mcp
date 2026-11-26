//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestStartJobIsolatedRefusedThroughSymlinkedStateDir: the state directory can
// reach a git work tree only through a symlink, so the lexical path of the work
// directory has no git ancestor and only the symlink-resolved walk finds it.
// The refusal must name the resolved repository root.
func TestStartJobIsolatedRefusedThroughSymlinkedStateDir(t *testing.T) {
	clean := t.TempDir()
	testutil.SkipIfInGitWorkTree(t, clean)
	repo := t.TempDir()
	mkdirAll(t, filepath.Join(repo, ".git"))
	mkdirAll(t, filepath.Join(repo, "sub"))
	link := filepath.Join(clean, "link")
	if err := os.Symlink(filepath.Join(repo, "sub"), link); err != nil {
		t.Fatal(err)
	}
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{stateDir: link, agyPath: "/usr/bin/agy"})

	_, err = m.StartJob(StartRequest{Prompt: "x", Isolated: true})
	if err == nil {
		t.Fatal("StartJob must refuse an isolated run whose state directory resolves into a git work tree")
	}
	if !strings.Contains(err.Error(), "git work tree") || !strings.Contains(err.Error(), resolvedRepo) {
		t.Errorf("error = %v, want it to name the git work tree %s", err, resolvedRepo)
	}
}

// TestGitAncestorFailsClosedOnUnreadableAncestor: a directory that cannot be
// searched makes the .git probe fail with an error other than "does not exist",
// and gitAncestor returns that error instead of reporting no repository.
func TestGitAncestorFailsClosedOnUnreadableAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can search a directory with no permissions")
	}
	base := t.TempDir()
	mid := filepath.Join(base, "mid")
	start := filepath.Join(mid, "sub")
	mkdirAll(t, start)
	if err := os.Chmod(mid, 0); err != nil {
		t.Fatal(err)
	}
	// Registered after TempDir's own cleanup, so it runs first and the removal
	// of base can descend into mid again.
	t.Cleanup(func() { _ = os.Chmod(mid, 0o700) })

	root, found, err := gitAncestor(start)
	if err == nil || found || root != "" {
		t.Fatalf("gitAncestor = (%q, %v, %v), want an error and no repository", root, found, err)
	}
}
