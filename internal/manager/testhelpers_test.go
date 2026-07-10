package manager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
)

// managerOpts configures newManager's config.Config beyond its sensible
// zero-value defaults (StateDir: t.TempDir(), MaxConcurrency: 4).
type managerOpts struct {
	stateDir       string // "" -> t.TempDir()
	agyPath        string
	supervisorExe  string
	maxConcurrency int // 0 -> 4
	defaultTimeout time.Duration
	defaultModel   string
	jobTTL         time.Duration
	withCacheFile  bool // true -> m.cacheFile = a fresh t.TempDir()/last_conversations.json
}

// newManager builds a *Manager for tests, consolidating the suite's several
// ad hoc builders (newTestManager, newManagerForRestore, twoManagers) into
// one parameterized constructor.
func newManager(t *testing.T, opts managerOpts) *Manager {
	t.Helper()
	stateDir := opts.stateDir
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	maxConcurrency := opts.maxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = 4
	}
	m := New(config.Config{
		AgyPath:        opts.agyPath,
		SupervisorExe:  opts.supervisorExe,
		StateDir:       stateDir,
		DefaultTimeout: opts.defaultTimeout,
		DefaultModel:   opts.defaultModel,
		MaxConcurrency: maxConcurrency,
		JobTTL:         opts.jobTTL,
	})
	if opts.withCacheFile {
		m.cacheFile = filepath.Join(t.TempDir(), "last_conversations.json")
	}
	return m
}
