// Package config resolves agy-mcp runtime configuration from environment and defaults.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Config holds resolved runtime settings.
type Config struct {
	AgyPath        string        // path to the agy binary
	SupervisorExe  string        // path to the agy-mcp binary used as the run-job supervisor
	StateDir       string        // root of the on-disk job store
	DefaultModel   string        // empty means let agy use its configured default
	DefaultTimeout time.Duration // hard per-job timeout
	MaxConcurrency int           // global cap on concurrent jobs
	JobTTL         time.Duration // age after which finished jobs are GC'd
	HTTPToken      string        // optional bearer token for HTTP mode; empty = unauthenticated

	// ConversationCacheFile overrides where agy's conversation cache
	// (last_conversations.json) is read from. Empty means agy's default
	// location under the user's home. Primarily a test seam.
	ConversationCacheFile string
}

// baseConfig returns a Config carrying exactly the defaults shared by Resolve
// and ResolveWait (DefaultTimeout, MaxConcurrency, JobTTL). It is the single
// source of those defaults, so the two resolvers cannot drift apart.
func baseConfig() Config {
	return Config{
		DefaultTimeout: 30 * time.Minute,
		MaxConcurrency: 4,
		JobTTL:         24 * time.Hour,
	}
}

// Resolve builds a Config from environment variables and defaults.
func Resolve() (Config, error) {
	c := baseConfig()
	c.DefaultModel = os.Getenv("AGY_MCP_DEFAULT_MODEL")
	c.HTTPToken = os.Getenv("AGY_MCP_HTTP_TOKEN")

	if p := os.Getenv("AGY_MCP_AGY_PATH"); p != "" {
		// Resolve the override with LookPath, symmetric with the PATH branch below, so
		// a typo, a non-executable file, or a bad PATH-relative name fails fast at
		// startup instead of only at exec time on the first job. LookPath also handles
		// a bare name (PATH lookup).
		resolved, err := exec.LookPath(p)
		if err != nil {
			return Config{}, fmt.Errorf("AGY_MCP_AGY_PATH %q: %w", p, err)
		}
		c.AgyPath = resolved
	} else {
		p, err := exec.LookPath("agy")
		if err != nil {
			return Config{}, fmt.Errorf("agy not found on PATH; set AGY_MCP_AGY_PATH: %w", err)
		}
		c.AgyPath = p
	}
	// agy runs under the supervisor with cmd.Dir set to the job's cwd, so AgyPath
	// must be absolute or it would resolve against the wrong directory; LookPath can
	// return a relative path (a relative override, or a relative PATH entry). Report
	// the pre-Abs value on failure since Abs returns "" then.
	abs, err := filepath.Abs(c.AgyPath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve agy path %q: %w", c.AgyPath, err)
	}
	c.AgyPath = abs

	self, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("resolve own executable: %w", err)
	}
	c.SupervisorExe = self

	stateRoot, err := resolveStateDir()
	if err != nil {
		return Config{}, err
	}
	c.StateDir = stateRoot

	return c, nil
}

// resolveStateDir returns the job-state root: AGY_MCP_STATE_DIR verbatim, or
// the XDG state-home fallback. Shared by Resolve and ResolveWait so the two
// cannot drift.
func resolveStateDir() (string, error) {
	if stateRoot := os.Getenv("AGY_MCP_STATE_DIR"); stateRoot != "" {
		return stateRoot, nil
	}
	xdg := os.Getenv("XDG_STATE_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		xdg = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(xdg, "agy-mcp"), nil
}

// ResolveWait builds the minimal Config the wait-only subcommands (wait-job,
// hook-wait) need: the state dir, defaults, and its own executable path, with
// no agy binary lookup. Reading job status never execs agy, so requiring it on
// PATH would be an artificial failure for a pure observer. SupervisorExe is
// still resolved (unlike AgyPath): processAlive's fallback liveness check
// compares a job's recorded supervisor process name against
// m.cfg.SupervisorExe, and the wait subcommands run from the same agy-mcp
// binary that supervises jobs in the normal single-install case, so leaving
// it empty would make that fallback compare against filepath.Base(""), which
// can never match a real comm value and would misreport a live job as dead.
func ResolveWait() (Config, error) {
	stateDir, err := resolveStateDir()
	if err != nil {
		return Config{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("resolve own executable: %w", err)
	}
	c := baseConfig()
	c.StateDir = stateDir
	c.SupervisorExe = self
	return c, nil
}
