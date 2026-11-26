package manager

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestGitAncestor pins the walk that refuses an isolated run under a git work
// tree. The walk starts at base/a/b and each row puts its marker at a
// different level, or none.
// A /.git row is not tested: the loop checks a directory's .git before testing
// whether it is the filesystem root, so the root is covered by construction.
func TestGitAncestor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string // path of the .git entry under base; "" for none
		file   bool   // the marker is a regular file instead of a directory
		root   string // the directory under base that must be reported; "" for none
	}{
		{name: "git directory in the parent", marker: filepath.Join("a", ".git"), root: "a"},
		{name: "git directory two levels up", marker: ".git", root: "."},
		{name: "git file, the worktree and submodule shape", marker: ".git", file: true, root: "."},
		{name: "no git entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			start := filepath.Join(base, "a", "b")
			mkdirAll(t, start)
			switch {
			case tc.marker == "":
				testutil.SkipIfInGitWorkTree(t, base)
			case tc.file:
				if err := os.WriteFile(filepath.Join(base, tc.marker), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				mkdirAll(t, filepath.Join(base, tc.marker))
			}
			wantRoot := ""
			if tc.root != "" {
				wantRoot = filepath.Join(base, tc.root)
			}

			root, found, err := gitAncestor(start)
			if err != nil {
				t.Fatalf("gitAncestor: %v", err)
			}
			if found != (wantRoot != "") || root != wantRoot {
				t.Fatalf("gitAncestor = (%q, %v), want (%q, %v)", root, found, wantRoot, wantRoot != "")
			}
		})
	}
}

// TestPrepareWorkDirRefusesExistingDirectory: the work directory is created,
// never reused, so prepareWorkDir refuses a path that already exists, here one
// holding an AGENTS.md.
func TestPrepareWorkDirRefusesExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	mkdirAll(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkDir(dir); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("prepareWorkDir(existing %s) = %v, want an error wrapping fs.ErrExist", dir, err)
	}
}

// TestRunDirRejectsInvalidJobID: an isolated run's directory comes from the
// store, so an id the store refuses yields its error, never a path outside the
// store root.
func TestRunDirRejectsInvalidJobID(t *testing.T) {
	m := newManager(t, managerOpts{defaultTimeout: time.Minute})
	if dir, err := m.runDir(StartRequest{Isolated: true}, "../escape"); !errors.Is(err, jobstore.ErrInvalidID) || dir != "" {
		t.Fatalf("runDir(isolated, ../escape) = %q, %v; want an error wrapping jobstore.ErrInvalidID", dir, err)
	}
}

// TestStartJobIsolatedRefusedUnderGitWorkTree: an empty directory inside a git
// work tree gets that repository's rule files and hooks (MEASURED against agy
// 1.5.18 on macOS), so an isolated run whose job directory sits under one must
// be refused. The conversation id and idempotency key are what make the gate and
// claim assertions meaningful: without them no key is held and no claim is taken.
func TestStartJobIsolatedRefusedUnderGitWorkTree(t *testing.T) {
	if !proc.Supported {
		t.Skip("StartJob refuses on platforms without job supervision before it reaches the work directory")
	}
	repo := t.TempDir()
	mkdirAll(t, filepath.Join(repo, ".git"))
	m := newManager(t, managerOpts{stateDir: filepath.Join(repo, "state"), agyPath: "/usr/bin/agy"})

	_, err := m.StartJob(StartRequest{
		Prompt: "x", Isolated: true, ConversationID: "c1", IdempotencyKey: "k1",
	})
	if err == nil {
		t.Fatal("StartJob must refuse an isolated run under a git work tree")
	}
	if !strings.Contains(err.Error(), "git work tree") || !strings.Contains(err.Error(), "AGY_MCP_STATE_DIR") {
		t.Errorf("error = %v, want it to name the git work tree and AGY_MCP_STATE_DIR", err)
	}
	ids, lerr := m.store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(ids) != 0 {
		t.Errorf("store holds %v, want no job: the refused job dir must be removed", ids)
	}
	if m.gate.inFlight != 0 {
		t.Errorf("in-flight = %d, want 0", m.gate.inFlight)
	}
	if len(m.gate.keys) != 0 {
		t.Errorf("gate holds %v, want no keys", m.gate.keys)
	}
	if len(m.idemKeys) != 0 {
		t.Errorf("idempotency claims = %v, want none", m.idemKeys)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
