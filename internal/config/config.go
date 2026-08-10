// Package config resolves agy-mcp runtime configuration from environment and defaults.
package config

import (
	"errors"
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
	DefaultModel   string        // a model id (see list_models); empty means let agy use its configured default
	DefaultTimeout time.Duration // hard per-job timeout
	MaxConcurrency int           // global cap on concurrent jobs
	JobTTL         time.Duration // age after which finished jobs are GC'd
	HTTPToken      string        // optional bearer token for HTTP mode; empty = unauthenticated

	// ConversationIDWait bounds how long Manager.AwaitConversationID blocks waiting
	// for agy to name the conversation it created, so a fresh agy_run can report a
	// real conversation id instead of an empty one. It is a field rather than a
	// constant so that a test whose fake supervisor records no conversation id
	// does not pay the full budget on every agy_run. Zero disables the wait.
	ConversationIDWait time.Duration

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
		DefaultTimeout:     30 * time.Minute,
		MaxConcurrency:     4,
		JobTTL:             24 * time.Hour,
		ConversationIDWait: DefaultConversationIDWait,
	}
}

// DefaultConversationIDWait is how long agy_run waits for agy's init event to name the
// conversation. agy emits it before any model work but after its own startup,
// which includes launching whatever MCP servers it is configured with, so the
// budget is generous relative to the roughly one second observed against a warm
// 1.1.8. Expiry is not a failure: agy_status reports the id once the event
// lands.
const DefaultConversationIDWait = 2 * time.Second

// Resolve builds a Config from environment variables and defaults.
func Resolve() (Config, error) {
	c := baseConfig()
	c.DefaultModel = os.Getenv("AGY_MCP_DEFAULT_MODEL")
	c.HTTPToken = os.Getenv("AGY_MCP_HTTP_TOKEN")

	// The two lookup branches fail differently, on purpose.
	//
	// An explicit AGY_MCP_AGY_PATH is a claim about one specific binary, so a typo,
	// a non-executable file, or a bad PATH-relative name fails fast at startup
	// rather than surfacing on the first job.
	//
	// A bare PATH miss is not an error at all. initialize, tools/list, and
	// list_sessions never exec agy, so refusing to start would deny a client the
	// whole introspection surface over a binary it may not be about to use, and
	// would make the server unrunnable in a container that has no agy at all.
	// AgyPath is left empty and AgyBinary retries the lookup at exec time, where a
	// genuinely missing agy becomes a clear per-call error instead.
	if p := os.Getenv("AGY_MCP_AGY_PATH"); p != "" {
		resolved, err := lookupAgy(p)
		if err != nil {
			return Config{}, fmt.Errorf("AGY_MCP_AGY_PATH %q: %w", p, err)
		}
		c.AgyPath = resolved
	} else if resolved, err := lookupAgy("agy"); err == nil {
		c.AgyPath = resolved
	}

	self, err := resolveSupervisorExe()
	if err != nil {
		return Config{}, err
	}
	c.SupervisorExe = self

	stateRoot, err := resolveStateDir()
	if err != nil {
		return Config{}, err
	}
	c.StateDir = stateRoot

	return c, nil
}

// AgyBinary returns the absolute path to the agy binary. Every site that execs
// agy must go through it rather than reading AgyPath directly.
//
// AgyPath is empty when agy was not on PATH at startup, because Resolve defers
// that lookup instead of refusing to start (see Resolve). The lookup is retried
// here, at the moment agy is actually needed, so the error lands on the tool
// call that needs it. A side benefit: an agy installed after the server started
// is picked up without a restart.
//
// A path already resolved at startup is returned unchanged. Re-running LookPath
// per job would let a mid-session PATH change swap the binary under a running
// server, which is exactly the ambiguity the startup resolution removes.
func (c Config) AgyBinary() (string, error) {
	if c.AgyPath != "" {
		return c.AgyPath, nil
	}
	p, err := lookupAgy("agy")
	if err != nil {
		return "", pathLookupGuidance(err)
	}
	return p, nil
}

// lookupAgy resolves an agy name or path to an absolute executable path. It is
// shared by Resolve and AgyBinary so the two can never disagree about what
// counts as a usable binary. Absolutizing is not optional: agy runs under the
// supervisor with cmd.Dir set to the job's cwd, so a relative result (a relative
// override, or a relative PATH entry) would resolve against the wrong directory.
// Report the pre-Abs value on failure since Abs returns "" then.
//
// The LookPath error is returned unwrapped. Each caller knows what the name it
// passed meant and adds guidance to match: an explicit override that fails is a
// different problem from a bare PATH miss, and telling someone who just set
// AGY_MCP_AGY_PATH to set AGY_MCP_AGY_PATH points them away from the real fault.
func lookupAgy(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve agy path %q: %w", p, err)
	}
	return abs, nil
}

// pathLookupGuidance annotates a failed bare-name PATH lookup with the fix.
//
// exec.ErrDot is called out separately because it is not a missing binary at
// all: agy was found, through a relative PATH entry, and Go refuses to run it
// because the result would depend on the caller's working directory. That is
// precisely the situation a job hits, since each one runs with cmd.Dir set to
// its own cwd, so reporting it as "not found" would send the reader hunting for
// an installation that is already there.
func pathLookupGuidance(err error) error {
	if errors.Is(err, exec.ErrDot) {
		return fmt.Errorf("agy is on PATH only through a relative entry, which cannot be used because each job runs in its own working directory; set AGY_MCP_AGY_PATH to an absolute path: %w", err)
	}
	return fmt.Errorf("agy not found on PATH; set AGY_MCP_AGY_PATH: %w", err)
}

// resolveStateDir returns the job-state root: AGY_MCP_STATE_DIR (made absolute)
// or the XDG state-home fallback. Shared by Resolve and ResolveWait so the two
// cannot drift.
func resolveStateDir() (string, error) {
	stateRoot := os.Getenv("AGY_MCP_STATE_DIR")
	if stateRoot == "" {
		xdg := os.Getenv("XDG_STATE_HOME")
		if xdg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home: %w", err)
			}
			xdg = filepath.Join(home, ".local", "state")
		}
		stateRoot = filepath.Join(xdg, "agy-mcp")
	}
	// A relative override resolves against each process's own cwd; hook-wait runs
	// from the session cwd while the server does not, so the same relative value
	// would silently split the job store. Absolutize so every consumer agrees,
	// covering both the override and the XDG fallback.
	abs, err := filepath.Abs(stateRoot)
	if err != nil {
		return "", fmt.Errorf("resolve state dir %q: %w", stateRoot, err)
	}
	return abs, nil
}

// resolveSupervisorExe returns the path to the running agy-mcp binary, used as
// the run-job supervisor. Shared by Resolve and ResolveWait so the two cannot
// drift.
func resolveSupervisorExe() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable: %w", err)
	}
	return self, nil
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
	self, err := resolveSupervisorExe()
	if err != nil {
		return Config{}, err
	}
	c := baseConfig()
	c.StateDir = stateDir
	c.SupervisorExe = self
	// Cheap env reads carried for uniformity with Resolve; the wait paths do not
	// consume them today.
	c.DefaultModel = os.Getenv("AGY_MCP_DEFAULT_MODEL")
	c.HTTPToken = os.Getenv("AGY_MCP_HTTP_TOKEN")
	// One AGY_MCP_ variable is read nowhere in this package at all:
	// AGY_MCP_WAIT_READY_FILE, which the wait subcommands read in package main (see
	// waitReadyFileEnv). It is consumed at the instant their signal handler is
	// installed, which is past the point a resolved Config is threaded to, so
	// routing it through here would buy uniformity at the cost of carrying the
	// value through two more signatures. Noted so an audit of the env surface from
	// this file is not misled. (AGY_MCP_AGY_PATH is also absent here, but for the
	// reason this function's own doc gives: the wait paths never exec agy.)
	return c, nil
}
