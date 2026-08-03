package manager

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// crossLock provides per-key cross-process advisory locking via flock(2), so the
// gate's per-key serialization holds across separate agy-mcp processes that share
// one AGY_MCP_STATE_DIR. That sharing is the default in stdio mode, where every MCP
// client session spawns its own agy-mcp process: the in-process gate (concurrency.go)
// stops same-key concurrency within one process, and this stops it across
// processes, closing the agy session-lock hang that the gate alone cannot prevent
// on a shared state dir.
//
// A lock is held by the manager process for the whole run: acquired alongside the
// gate slot and released when the job ends. The held fd is kept open and stored
// here; closing it drops the flock. Lock files are never unlinked, because removing
// a flock file races (one process unlinks while another recreates, and both then
// hold locks on different inodes for the same path), so the empty files are left in
// place. They accumulate one per distinct conversation ever locked, each zero
// bytes.
type crossLock struct {
	dir string
	mu  sync.Mutex
	fds map[string]*os.File // key -> open lock file; one entry per key this process holds
}

func newCrossLock(stateDir string) *crossLock {
	return &crossLock{dir: filepath.Join(stateDir, "locks"), fds: map[string]*os.File{}}
}

// lockPath maps a gate key to its lock file, as <locks dir>/<sha256 of key>.lock.
//
// The hash is load-bearing, not cosmetic. The key is "conv:" plus a conversation
// id (keyFor), and nothing validates that id anywhere on the way here: it arrives
// raw off the MCP wire, or out of agy's own cache via resolveLatest, or replayed
// from meta.json on restore. Hashing is what turns that arbitrary string into one
// safe, unique, fixed-length filename. So it cannot escape the locks dir (an id
// carrying enough ".." would otherwise resolve to a path outside it; how far out
// depends on the state dir's depth, and whether the open then succeeds or merely
// fails somewhere it should never have reached is not the point); cannot alias onto
// another key's file, which matters because NTFS and a default APFS or HFS+ volume
// are case-insensitive, so an unhashed "conv:ABC" and "conv:abc" would land on one
// lock file; cannot outgrow a filesystem's name limit; and is a legal filename
// everywhere, which the raw key is not: the "conv:" colon selects an alternate data
// stream on Windows rather than naming a file.
// Hashing is also deterministic, so sibling processes derive one path from one key
// without agreeing on an escaping scheme. Validate the id at the entry point and
// the escape half stops being the only guard; until then it is.
//
// No worked example of the length case, and none of Windows' name-mangling ones
// (trailing dots and spaces, reserved device names), on purpose. Those arguments
// fit validJobID (jobstore), where the id IS the whole path component, and they do
// not transplant to here, where the component is the hash plus ".lock"; derive any
// concrete case for "conv:"+id+".lock" rather than for the id on its own. The
// case-folding example above survives that test, which is why it is stated.
//
// The empty key never reaches here: both routes into tryLock (admit and
// forceAdmit) return before it.
func (c *crossLock) lockPath(key string) string {
	return filepath.Join(c.dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(key))))
}

// tryLock takes the cross-process lock for key without blocking. It returns
// (true, nil) when the lock is acquired (the fd is retained until unlock),
// (false, nil) when another process already holds it, and (false, err) on any other
// failure, which the caller must treat as fail-closed: refuse the run rather than
// proceed without cross-process exclusion. The gate never admits the same key twice
// in one process, so tryLock is not expected to be called for a key this instance
// already holds; the duplicate guard below enforces that invariant locally (rather
// than silently overwriting and leaking the prior fd) should a future caller bypass
// the gate.
func (c *crossLock) tryLock(key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.fds[key]; held {
		return false, fmt.Errorf("crosslock: key %q already held by this process", key)
	}
	// MkdirAllDurable, not a bare os.MkdirAll: when this is the first path to create
	// the shared state root (a keyed job before any Create, admit runs before Create),
	// it fsyncs the root's own directory entry so a crash cannot lose it (issue #119).
	// It shares the job store's durable-create logic; on a directory-fsync-hostile
	// filesystem the fsyncs are logged and swallowed, so it still succeeds like a
	// bare MkdirAll.
	if err := jobstore.MkdirAllDurable(c.dir, 0o700); err != nil {
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
	// Closing the fd drops the flock. A close error should never happen for a lock
	// file, but if it does the kernel may not have released the lock yet while this
	// process has already forgotten the fd; log it so the regression surfaces rather
	// than silently leaving the key un-releasable until process exit (which does free
	// the lock).
	if err := f.Close(); err != nil {
		log.Printf("crosslock: close lock file for key %q: %v", key, err)
	}
}
