package manager

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// TestDoctorReportsMissingAgy: with no agy resolvable, the binary check fails and
// the whole report is not OK, so the command exits non-zero. Other checks still
// run (the report names every problem at once), which is why this asserts on the
// binary check specifically rather than just report.OK().
func TestDoctorReportsMissingAgy(t *testing.T) {
	noAgyOnPath(t)
	m := newManager(t, managerOpts{})

	report := m.Doctor(t.Context())
	if report.OK() {
		t.Fatal("Doctor reported OK with no agy resolvable")
	}
	bin := findCheck(t, report, checkAgyBinaryName)
	if bin.Status != CheckFail {
		t.Errorf("agy binary check = %v, want FAIL when agy cannot be resolved", bin.Status)
	}
}

// TestDoctorConfigNamesSourcesWithoutSecret: the config check must name where the
// HTTP token came from without ever printing its value.
func TestDoctorConfigNamesSourcesWithoutSecret(t *testing.T) {
	const secret = "super-secret-token-value"
	m := New(config.Config{
		AgyPath:      "/nonexistent/agy",
		StateDir:     t.TempDir(),
		HTTPToken:    secret,
		DefaultModel: "gemini-3.1-pro-high",
	})
	got := m.checkConfigSources()
	if got.Status != CheckPass {
		t.Fatalf("config check status = %v, want PASS", got.Status)
	}
	if strings.Contains(got.Detail, secret) {
		t.Fatalf("config detail leaked the token value:\n%s", got.Detail)
	}
	// It must still report that a token IS set, so a reader can tell auth is on.
	// Assert on the source label AGY_MCP_HTTP_TOKEN, which the set branch emits and
	// the unset branch does not: a bare "set" would also match the "agy default
	// (unset)" line this same output carries, so it could not tell set from unset.
	if !strings.Contains(got.Detail, "http token") || !strings.Contains(got.Detail, "AGY_MCP_HTTP_TOKEN") {
		t.Fatalf("config detail should report the token as set via its source label (without the value):\n%s", got.Detail)
	}
}

// TestDoctorStaleJobsWarn: a job whose supervisor is gone and that recorded no
// exit code is an orphan a prior crash left; the jobs check must WARN and name
// it, while a cleanly finished job (an exit code present) is not counted.
func TestDoctorStaleJobsWarn(t *testing.T) {
	m := newManager(t, managerOpts{})
	// A dead supervisor: PID that is not this process, under a boot id that cannot
	// match, and no exit code written, so processAlive is false and ExitCode absent.
	if _, err := m.store.Create(jobstore.Meta{ID: "orphan", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"}); err != nil {
		t.Fatal(err)
	}
	// A finished job: same shape but with an exit code, so it is NOT stale.
	if _, err := m.store.Create(jobstore.Meta{ID: "done", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"}); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("done", 0); err != nil {
		t.Fatal(err)
	}

	got := m.checkStaleJobs()
	if got.Status != CheckWarn {
		t.Fatalf("jobs check = %v (%s), want WARN", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "orphan") {
		t.Errorf("jobs detail should name the orphan id, got: %s", got.Detail)
	}
	if strings.Contains(got.Detail, "done") {
		t.Errorf("jobs detail must not list the cleanly finished job, got: %s", got.Detail)
	}
}

// TestDoctorStaleJobsPassWhenClean: a store with only finished jobs (or none) is
// healthy, so the check passes and cannot flip the exit code. A WARN does not
// fail the report, but a PASS is what a fresh install should see.
func TestDoctorStaleJobsPassWhenClean(t *testing.T) {
	m := newManager(t, managerOpts{})
	got := m.checkStaleJobs()
	if got.Status != CheckPass {
		t.Fatalf("jobs check on an empty store = %v (%s), want PASS", got.Status, got.Detail)
	}
}

// TestDoctorStateDirFreshInstallPasses: a state dir that does not exist yet but
// sits under a real writable directory is healthy, because the server creates it
// on first use. This is the fresh-install path, and it is the one that exercises
// the os.IsNotExist arm and nearestExistingDir (walking up past the missing
// intermediate segments to the writable temp root).
func TestDoctorStateDirFreshInstallPasses(t *testing.T) {
	// Two missing segments so nearestExistingDir actually has to walk up, not just
	// stat the immediate parent.
	missing := filepath.Join(t.TempDir(), "not-yet", "jobs")
	m := New(config.Config{AgyPath: "/nonexistent/agy", StateDir: missing})
	got := m.checkStateDir()
	if got.Status != CheckPass {
		t.Fatalf("state dir check = %v (%s), want PASS for a not-yet-created dir under a writable parent", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "will be created") {
		t.Errorf("detail should say the dir will be created on first run, got: %s", got.Detail)
	}
}

// The FAIL branches of checkStateDir (an unwritable parent on the fresh-install
// arm, and a stat error under a regular file on the catch-all arm) depend on Unix
// filesystem semantics that Windows does not share, so they live in
// doctor_posix_test.go. The cross-platform healthy fresh-install case is above.

// TestCheckStatusString pins the label each status renders, including the
// out-of-range default that guards against a new CheckStatus printed as a bare
// integer in the report.
func TestCheckStatusString(t *testing.T) {
	for _, tc := range []struct {
		s    CheckStatus
		want string
	}{
		{CheckPass, "PASS"},
		{CheckWarn, "WARN"},
		{CheckFail, "FAIL"},
		{CheckStatus(99), "????"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("CheckStatus(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestDoctorConfigSourcesFromEnv covers the env-override branches of
// checkConfigSources: when the AGY_MCP_* variables are set, each setting's source
// is named by its variable rather than the default label, still without a value
// for the token.
func TestDoctorConfigSourcesFromEnv(t *testing.T) {
	t.Setenv("AGY_MCP_AGY_PATH", "/env/agy")
	t.Setenv("AGY_MCP_STATE_DIR", "/env/state")
	t.Setenv("AGY_MCP_DEFAULT_MODEL", "gemini-x")
	const secret = "s3cr3t-http-value"
	m := New(config.Config{AgyPath: "/env/agy", StateDir: "/env/state", DefaultModel: "gemini-x", HTTPToken: secret})
	got := m.checkConfigSources()
	if got.Status != CheckPass {
		t.Fatalf("status = %v (%s), want PASS", got.Status, got.Detail)
	}
	for _, want := range []string{"AGY_MCP_AGY_PATH", "AGY_MCP_STATE_DIR", "AGY_MCP_DEFAULT_MODEL"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail should name source %s, got: %s", want, got.Detail)
		}
	}
	if strings.Contains(got.Detail, secret) {
		t.Errorf("detail leaked the token value:\n%s", got.Detail)
	}
}

// findCheck returns the named check or fails the test if the report lacks it.
func findCheck(t *testing.T, r DoctorReport, name string) CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no %q check; checks: %v", name, r.Checks)
	return CheckResult{}
}
