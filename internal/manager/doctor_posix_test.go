//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestDoctorHealthy: a resolvable, current, reachable agy and a writable state
// dir yield an all-passing report that exits zero. It execs the fake agy for the
// model listing (the auth/reachability check), so it is posix-gated like the
// other WriteFakeAgy tests.
func TestDoctorHealthy(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{
		Models: []testutil.FakeModel{{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"}},
	})
	m := newManager(t, managerOpts{agyPath: agy})

	report := m.Doctor(t.Context())
	if !report.OK() {
		for _, c := range report.Checks {
			t.Logf("[%s] %s: %s", c.Status, c.Name, c.Detail)
		}
		t.Fatal("Doctor should report OK for a healthy install")
	}
	// The reachability check must actually have listed the fake's model, proving it
	// execed agy rather than passing vacuously.
	reach := findCheck(t, report, checkAgyReachableName)
	if reach.Status != CheckPass {
		t.Errorf("reachability check = %v (%s), want PASS", reach.Status, reach.Detail)
	}
}

// TestDoctorStaleJobsFailsWhenStoreUnreadable: a List() error is an unreadable
// store root, the fail-closed condition the server itself refuses to start on, so
// the jobs check must FAIL (not merely WARN) and drag the exit code non-zero.
//
// Posix-only and root-guarded: it makes the jobs dir unreadable with mode 0o000,
// which only denies access to a non-root user on a POSIX filesystem.
func TestDoctorStaleJobsFailsWhenStoreUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions, so List would succeed")
	}
	stateDir := t.TempDir()
	jobsDir := filepath.Join(stateDir, "jobs") // Store.List reads <stateDir>/jobs
	if err := os.Mkdir(jobsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore a mode t.TempDir()'s cleanup can traverse and remove.
	t.Cleanup(func() { _ = os.Chmod(jobsDir, 0o700) })

	m := New(config.Config{AgyPath: "/nonexistent/agy", StateDir: stateDir})
	got := m.checkStaleJobs()
	if got.Status != CheckFail {
		t.Fatalf("jobs check = %v (%s), want FAIL when the store root is unreadable", got.Status, got.Detail)
	}
}

// TestDoctorStateDirFreshInstallParentUnwritable: the fresh-install path FAILs when
// the nearest existing ancestor cannot be written, because creation would fail. It
// reaches the same os.IsNotExist arm as the passing case but takes the probe-fails
// branch.
//
// Posix-only: it relies on a 0o500 directory actually denying writes, which Windows
// does not enforce through Unix mode bits (there the probe would succeed and the
// check would PASS). Skipped as root, which bypasses the mode bits too.
func TestDoctorStateDirFreshInstallParentUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions, so the probe would succeed")
	}
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil { // r-x, not writable
		t.Fatal(err)
	}
	m := New(config.Config{AgyPath: "/nonexistent/agy", StateDir: filepath.Join(parent, "jobs")})
	got := m.checkStateDir()
	if got.Status != CheckFail {
		t.Fatalf("state dir check = %v (%s), want FAIL when the parent is not writable", got.Status, got.Detail)
	}
	// Pin the branch: this must be the fresh-install (IsNotExist) arm reporting an
	// unwritable parent, not some other FAIL.
	if !strings.Contains(got.Detail, "not writable") {
		t.Errorf("detail should name the unwritable parent, got: %s", got.Detail)
	}
}

// TestDoctorStateDirStatError: a state dir path whose parent is a regular file lands
// on the catch-all stat-error arm and fails. This pins that arm separately from the
// fresh-install one.
//
// Posix-only: it relies on os.Stat returning ENOTDIR for a path under a regular file,
// which os.IsNotExist does NOT treat as "not exist", so the code reaches the default
// arm. Windows instead reports a path-not-found error that os.IsNotExist treats as
// true, routing the same input through the fresh-install arm rather than this one.
func TestDoctorStateDirStatError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(config.Config{AgyPath: "/nonexistent/agy", StateDir: filepath.Join(blocker, "agy-mcp", "jobs")})
	got := m.checkStateDir()
	if got.Status != CheckFail {
		t.Fatalf("state dir check = %v (%s), want FAIL when the path's parent is a file", got.Status, got.Detail)
	}
	// Pin the branch: a stat error that is NOT os.IsNotExist lands on the catch-all
	// "cannot stat" arm, distinct from the fresh-install arm.
	if !strings.Contains(got.Detail, "cannot stat") {
		t.Errorf("detail should come from the stat-error arm, got: %s", got.Detail)
	}
}
