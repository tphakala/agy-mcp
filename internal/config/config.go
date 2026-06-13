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

// Resolve builds a Config from environment variables and defaults.
func Resolve() (Config, error) {
	c := Config{
		DefaultModel:   os.Getenv("AGY_MCP_DEFAULT_MODEL"),
		DefaultTimeout: 30 * time.Minute,
		MaxConcurrency: 4,
		JobTTL:         24 * time.Hour,
		HTTPToken:      os.Getenv("AGY_MCP_HTTP_TOKEN"),
	}

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

	stateRoot := os.Getenv("AGY_MCP_STATE_DIR")
	if stateRoot == "" {
		xdg := os.Getenv("XDG_STATE_HOME")
		if xdg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Config{}, fmt.Errorf("resolve home: %w", err)
			}
			xdg = filepath.Join(home, ".local", "state")
		}
		stateRoot = filepath.Join(xdg, "agy-mcp")
	}
	c.StateDir = stateRoot

	return c, nil
}
