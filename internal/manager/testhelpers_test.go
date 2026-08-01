package manager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
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
	withCacheFile  bool   // true -> m.cacheFile = a fresh t.TempDir()/last_conversations.json
	agyVersion     string // "" -> a version that satisfies the gate; see newManager
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
	// Stub the version probe. The agyPath in these tests is usually a stand-in
	// that cannot be executed at all, so without this every StartJob would fail
	// in the gate rather than exercising what the test is about. A test that
	// wants to exercise the gate itself sets agyVersion.
	version := opts.agyVersion
	if version == "" {
		version = agyver.Required.String()
	}
	m.readAgyVersion = testutil.FakeVersion(version)
	// Pin the conversation-id wait at zero. It already is: this Config is a
	// literal, and config.DefaultConversationIDWait is applied only by
	// config.baseConfig, which Resolve and ResolveWait call and this helper does
	// not. Writing it down keeps a future switch to a resolved config here from
	// silently handing every test a real budget, against fake supervisors whose
	// progress file records no conversation id unless a fake agy supplies one. A
	// test that exercises the wait sets its own budget.
	m.conversationIDWait = 0
	return m
}
