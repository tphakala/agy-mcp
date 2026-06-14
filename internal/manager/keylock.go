package manager

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// crossLock provides per-key cross-process advisory locking via flock(2), so the
// gate's per-key serialization holds across separate agy-mcp processes that share
// one AGY_MCP_STATE_DIR. That sharing is the default in stdio mode, where every MCP
// client session spawns its own agy-mcp process: the in-process gate (concurrency.go)
// stops same-key concurrency within one process, and this stops it across processes,
// closing the agy session-lock hang and the conversation-cache misattribution the
// gate alone cannot prevent on a shared state dir.
//
// A lock is held by the manager process for the whole run: acquired alongside the
// gate slot and released when the job ends. The held fd is kept open and stored
// here; closing it drops the flock. Lock files are never unlinked, because removing
// a flock file races (one process unlinks while another recreates, and both then
// hold locks on different inodes for the same path), so the empty files are left in
// place. They accumulate one per distinct conversation/cwd ever locked, each zero
// bytes.
type crossLock struct {
	dir string
	mu  sync.Mutex
	fds map[string]*os.File // key -> open lock file; one entry per key this process holds
}

func newCrossLock(stateDir string) *crossLock {
	return &crossLock{dir: filepath.Join(stateDir, "locks"), fds: map[string]*os.File{}}
}

// lockPath maps a gate key to its lock file. The key (a conversation id or an
// absolute cwd) is hashed so an arbitrary path can neither escape the locks dir nor
// collide with another key, and so sibling processes derive the same path for the
// same key.
func (c *crossLock) lockPath(key string) string {
	return filepath.Join(c.dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(key))))
}

// tryLock takes the cross-process lock for key without blocking. It returns
// (true, nil) when the lock is acquired (the fd is retained until unlock),
// (false, nil) when another process already holds it, and (false, err) on any other
// failure, which the caller must treat as fail-closed: refuse the run rather than
// proceed without cross-process exclusion. The gate never admits the same key twice
// in one process, so tryLock is never called for a key this instance already holds
// (which would leak the prior fd).
func (c *crossLock) tryLock(key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return false, fmt.Errorf("create locks dir: %w", err)
	}
	f, err := os.OpenFile(c.lockPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, fmt.Errorf("open lock file: %w", err)
	}
	locked, err := flockExclusiveNB(f.Fd())
	if err != nil {
		_ = f.Close()
		return false, fmt.Errorf("flock: %w", err)
	}
	if !locked {
		_ = f.Close() // held by another process; drop our fd so it is not leaked
		return false, nil
	}
	c.fds[key] = f
	return true, nil
}

// unlock releases a key this instance holds by closing its fd, which drops the
// flock. It is a no-op for a key not held here, so a caller that did not acquire
// (because a sibling held it) can still call unlock unconditionally on release.
func (c *crossLock) unlock(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.fds[key]
	if !ok {
		return
	}
	delete(c.fds, key)
	_ = f.Close()
}
