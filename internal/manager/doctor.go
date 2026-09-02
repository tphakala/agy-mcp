package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
)

// Check names, shared by each check and its tests (see doctor_test.go, which
// looks checks up by these constants) so the two cannot drift, and a name is
// written once rather than at every CheckResult it appears in. All carry the
// Name suffix so none collides with the like-named check method.
const (
	checkAgyBinaryName    = "agy binary"
	checkAgyVersionName   = "agy version"
	checkAgyReachableName = "agy reachable (auth)"
	checkStateDirName     = "state dir"
	checkConfigName       = "config"
	checkJobsName         = "jobs"
)

// CheckStatus is a preflight check's verdict. Only CheckFail makes the doctor
// command exit non-zero; CheckWarn reports something worth seeing (stale jobs
// from a prior crash) without calling the install broken, so a fresh install
// with leftover state is not treated as a failure.
type CheckStatus int

const (
	CheckPass CheckStatus = iota
	CheckWarn
	CheckFail
)

// String renders the status as the fixed-width label the report prints.
func (s CheckStatus) String() string {
	switch s {
	case CheckPass:
		return "PASS"
	case CheckWarn:
		return "WARN"
	case CheckFail:
		return "FAIL"
	default:
		return "????"
	}
}

// CheckResult is one preflight check's outcome. Detail is a human-readable line
// (or lines) explaining the verdict and must never carry a secret value: the
// config check names where a setting came from, not what it is, so a token is
// reported as set/unset and never printed.
type CheckResult struct {
	Name   string
	Status CheckStatus
	Detail string
}

// DoctorReport is the full set of preflight checks, in the order they ran.
type DoctorReport struct {
	Checks []CheckResult
}

// OK reports whether the install is healthy: no check failed. A warning does not
// count as a failure, so the exit code a caller derives from this treats stale
// leftover jobs as information, not breakage.
func (r DoctorReport) OK() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return false
		}
	}
	return true
}

// Doctor runs the read-only preflight checks a run depends on and returns their
// results. It never mutates job state or starts a job: the one write it makes is
// a probe file in the state dir (removed immediately) to prove the dir is
// writable, which is the only way to actually verify that rather than guess from
// the mode bits.
//
// Every check runs regardless of earlier ones, so a report names every problem at
// once rather than stopping at the first: an operator fixing an install wants the
// whole list, not one error per re-run.
func (m *Manager) Doctor(ctx context.Context) DoctorReport {
	return DoctorReport{Checks: []CheckResult{
		m.checkAgyBinary(),
		m.checkAgyVersion(ctx),
		m.checkAgyReachable(ctx),
		m.checkStateDir(),
		m.checkConfigSources(),
		m.checkStaleJobs(),
	}}
}

// checkAgyBinary resolves the agy binary the same way every run does, so a PATH
// miss or a relative-only entry is reported here with the same guidance a job
// would get.
func (m *Manager) checkAgyBinary() CheckResult {
	agy, err := m.cfg.AgyBinary()
	if err != nil {
		return CheckResult{checkAgyBinaryName, CheckFail, err.Error()}
	}
	return CheckResult{checkAgyBinaryName, CheckPass, "found at " + agy}
}

// checkAgyVersion probes the version through the manager's own readAgyVersion
// seam (WaitDelay'd and ctx-checked) and compares it against the tracked floor,
// reusing agyver rather than duplicating the comparison. It reports the version
// it found so an operator sees how far off an outdated binary is, not just that
// it is outdated.
func (m *Manager) checkAgyVersion(ctx context.Context) CheckResult {
	agy, err := m.cfg.AgyBinary()
	if err != nil {
		return CheckResult{checkAgyVersionName, CheckFail, "cannot check: agy binary not resolved"}
	}
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	raw, err := m.readAgyVersion(ctx, agy)
	if err != nil {
		return CheckResult{checkAgyVersionName, CheckFail, err.Error()}
	}
	v, err := agyver.Parse(raw)
	if err != nil {
		return CheckResult{checkAgyVersionName, CheckFail, fmt.Sprintf("cannot parse version %q: %v", strings.TrimSpace(raw), err)}
	}
	if !v.AtLeast(agyver.Required) {
		return CheckResult{checkAgyVersionName, CheckFail, fmt.Sprintf("%s is below the required %s; upgrade agy", v, agyver.Required)}
	}
	return CheckResult{checkAgyVersionName, CheckPass, fmt.Sprintf("%s (>= required %s)", v, agyver.Required)}
}

// checkAgyReachable lists models, which is the cheapest call that requires a
// working, authenticated agy: it execs the real binary and fails if agy is
// missing, too old, or not logged in, so it doubles as the auth and reachability
// check. The error agy prints (an auth prompt, say) is surfaced in the detail.
func (m *Manager) checkAgyReachable(ctx context.Context) CheckResult {
	models, err := m.ListModels(ctx)
	if err != nil {
		return CheckResult{checkAgyReachableName, CheckFail, err.Error()}
	}
	return CheckResult{checkAgyReachableName, CheckPass, fmt.Sprintf("model listing reachable (%d model(s))", len(models))}
}

// checkStateDir verifies the job-store root is usable. An existing dir is proven
// writable by a probe file that is removed at once; a not-yet-created dir (a fresh
// install that has run no job) is healthy as long as the nearest existing
// ancestor is a writable directory, since the server creates the state dir on
// first use. Only a state dir that exists but cannot be written, or whose parent
// cannot be created into, is a failure.
func (m *Manager) checkStateDir() CheckResult {
	dir := m.cfg.StateDir
	if dir == "" {
		return CheckResult{checkStateDirName, CheckFail, "no state directory configured"}
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		if perr := probeWritable(dir); perr != nil {
			return CheckResult{checkStateDirName, CheckFail, fmt.Sprintf("%s exists but is not writable: %v", dir, perr)}
		}
		return CheckResult{checkStateDirName, CheckPass, "writable at " + dir}
	case err == nil:
		return CheckResult{checkStateDirName, CheckFail, dir + " exists but is not a directory"}
	case os.IsNotExist(err):
		// Fresh install: the dir will be created on first use. Confirm the nearest
		// existing ancestor is a writable directory so that creation will succeed.
		anc, aerr := nearestExistingDir(dir)
		if aerr != nil {
			return CheckResult{checkStateDirName, CheckFail, fmt.Sprintf("cannot resolve a parent of %s: %v", dir, aerr)}
		}
		if perr := probeWritable(anc); perr != nil {
			return CheckResult{checkStateDirName, CheckFail, fmt.Sprintf("%s does not exist and its parent %s is not writable: %v", dir, anc, perr)}
		}
		return CheckResult{checkStateDirName, CheckPass, dir + " will be created on first run (parent writable)"}
	default:
		return CheckResult{checkStateDirName, CheckFail, fmt.Sprintf("cannot stat %s: %v", dir, err)}
	}
}

// probeWritable proves a directory is writable by creating and removing a uniquely
// named file in it. Statting the mode bits is not enough: a dir can be mode 0755
// and still unwritable to this user, and a read-only mount reports writable bits.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".agy-mcp-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// nearestExistingDir walks up from a non-existent path to the first ancestor that
// exists, returning an error if it is not a directory. It stops at the filesystem
// root, which always exists.
func nearestExistingDir(dir string) (string, error) {
	for {
		parent := filepath.Dir(dir)
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", parent)
			}
			return parent, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if parent == dir {
			// filepath.Dir is idempotent at the root; guard against an infinite loop.
			return "", fmt.Errorf("reached filesystem root without finding an existing ancestor of %s", dir)
		}
		dir = parent
	}
}

// checkConfigSources names where each effective setting came from (an env var or
// the built-in default) without printing any secret value. The HTTP token in
// particular is reported as set or unset only, never echoed.
func (m *Manager) checkConfigSources() CheckResult {
	var b strings.Builder
	line := func(setting, source, value string) {
		fmt.Fprintf(&b, "\n    %-16s %s", setting+":", source)
		if value != "" {
			fmt.Fprintf(&b, " (%s)", value)
		}
	}
	// agy path: an explicit override vs a PATH lookup.
	if p := os.Getenv("AGY_MCP_AGY_PATH"); p != "" {
		line("agy path", "AGY_MCP_AGY_PATH", m.cfg.AgyPath)
	} else {
		line("agy path", "PATH lookup", m.cfg.AgyPath)
	}
	// default model: env override vs agy's own default (an empty DefaultModel).
	if os.Getenv("AGY_MCP_DEFAULT_MODEL") != "" {
		line("default model", "AGY_MCP_DEFAULT_MODEL", m.cfg.DefaultModel)
	} else {
		line("default model", "agy default (unset)", "")
	}
	// state dir: env override vs the XDG fallback.
	if os.Getenv("AGY_MCP_STATE_DIR") != "" {
		line("state dir", "AGY_MCP_STATE_DIR", m.cfg.StateDir)
	} else {
		line("state dir", "XDG default", m.cfg.StateDir)
	}
	// HTTP token: reported as set/unset only, never the value.
	if m.cfg.HTTPToken != "" {
		line("http token", "AGY_MCP_HTTP_TOKEN", "set")
	} else {
		line("http token", "unset (HTTP mode unauthenticated)", "")
	}
	return CheckResult{checkConfigName, CheckPass, strings.TrimPrefix(b.String(), "\n")}
}

// checkStaleJobs reports jobs the store still holds whose supervisor process is
// gone and that never recorded an exit code: orphans a prior crash left behind,
// which the periodic GC reaps on its own schedule but which are exactly what
// someone debugging odd behaviour wants to see now. It is a warning, not a
// failure: a healthy server GCs these in time, and a fresh install has none.
//
// It is strictly read-only, unlike GarbageCollect: it lists and inspects, and
// removes nothing.
//
// The staleness predicate here is deliberately simpler than gcEvaluate's, and the
// difference is intentional because this only REPORTS. gcEvaluate re-reads the
// exit code once after finding the supervisor dead (a race guard against a job
// that finished between the two reads) and separates a transient meta read error
// from a genuinely missing meta, because it is deciding whether to DELETE. A
// read-only WARN pays for neither: at worst it names a job that just finished, or
// a dir it momentarily could not read, and an operator eyeballing the list loses
// nothing by seeing it. Do not converge the two into a shared predicate on the
// strength of this: gcEvaluate's extra care exists for the deletion it drives.
func (m *Manager) checkStaleJobs() CheckResult {
	ids, err := m.store.List()
	if err != nil {
		return CheckResult{checkJobsName, CheckWarn, "cannot list job store: " + err.Error()}
	}
	var stale []string
	for _, id := range ids {
		meta, err := m.store.Load(id)
		if err != nil {
			// A dir with no readable meta.json is itself an orphan (a crash between
			// mkdir and the first meta write); count it as stale by id.
			stale = append(stale, id)
			continue
		}
		if _, done := m.store.ExitCode(id); done {
			continue // finished cleanly; not stale
		}
		if m.processAlive(meta) {
			continue // still running under a live supervisor
		}
		stale = append(stale, id)
	}
	if len(stale) == 0 {
		return CheckResult{checkJobsName, CheckPass, fmt.Sprintf("no stale jobs (%d total in store)", len(ids))}
	}
	slices.Sort(stale)
	return CheckResult{checkJobsName, CheckWarn, fmt.Sprintf("%d stale job(s) from a prior crash (GC reaps them): %s", len(stale), strings.Join(stale, ", "))}
}
