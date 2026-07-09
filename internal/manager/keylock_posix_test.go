//go:build linux || darwin

package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCrossLockExcludesAcrossInstances: two crossLock instances rooted at the
// same state dir model two sibling agy-mcp processes that share AGY_MCP_STATE_DIR.
// The second must be refused the key the first holds (the cross-process exclusion
// the in-process gate alone cannot provide), and must succeed once it is released.
func TestCrossLockExcludesAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	a := newCrossLock(dir)
	b := newCrossLock(dir)

	ok, err := a.tryLock("cwd:/w")
	if err != nil {
		t.Fatalf("a.tryLock: unexpected error %v", err)
	}
	if !ok {
		t.Fatal("a.tryLock should acquire a free key")
	}

	// A different instance (a sibling process) must not acquire the held key.
	ok, err = b.tryLock("cwd:/w")
	if err != nil {
		t.Fatalf("b.tryLock: unexpected error %v", err)
	}
	if ok {
		t.Fatal("b.tryLock must be refused while a holds the key")
	}

	// A distinct key is independent and still acquirable by the sibling.
	if ok, err := b.tryLock("cwd:/other"); err != nil || !ok {
		t.Fatalf("b.tryLock on a distinct key = (%v, %v), want (true, nil)", ok, err)
	}

	a.unlock("cwd:/w")

	// Released: the sibling can now take it.
	if ok, err := b.tryLock("cwd:/w"); err != nil || !ok {
		t.Fatalf("b.tryLock after a.unlock = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestCrossLockUnlockUnheldKeyIsNoop: unlocking a key this instance never held
// must not panic or disturb another holder. forceAdmit relies on this when a
// restored job's lock could not be re-acquired (a sibling holds it), so the later
// release must tolerate "no fd stored for this key".
func TestCrossLockUnlockUnheldKeyIsNoop(t *testing.T) {
	dir := t.TempDir()
	a := newCrossLock(dir)
	b := newCrossLock(dir)

	if ok, _ := a.tryLock("conv:x"); !ok {
		t.Fatal("a should acquire conv:x")
	}
	// b never held conv:x; unlocking it must be a harmless no-op (not release a's).
	b.unlock("conv:x")
	if ok, _ := b.tryLock("conv:x"); ok {
		t.Fatal("b must still be refused conv:x: a's hold survives b's no-op unlock")
	}
}

// TestCrossLockTryLockRejectsDuplicateHeldKey: locking a key this instance already
// holds is a contract violation (the gate prevents it in production). The guard must
// fail fast rather than overwrite and leak the prior fd, and the original hold must
// survive intact.
func TestCrossLockTryLockRejectsDuplicateHeldKey(t *testing.T) {
	c := newCrossLock(t.TempDir())
	if ok, err := c.tryLock("conv:x"); err != nil || !ok {
		t.Fatalf("first tryLock = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := c.tryLock("conv:x"); ok || err == nil {
		t.Fatalf("duplicate tryLock = (%v, %v), want (false, error)", ok, err)
	}
	// The original hold is intact: unlock then re-lock succeeds.
	c.unlock("conv:x")
	if ok, err := c.tryLock("conv:x"); err != nil || !ok {
		t.Fatalf("tryLock after unlock = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestCrossLockHashesArbitraryKeys: a gate key is an absolute cwd or a conversation
// id, so it can contain path separators and "..". Hashing the key into the filename
// must keep the lock file inside the locks dir (no path escape) and still serialize
// siblings, for both traversal-laden and very long keys.
func TestCrossLockHashesArbitraryKeys(t *testing.T) {
	dir := t.TempDir()
	a := newCrossLock(dir)
	b := newCrossLock(dir)

	for _, key := range []string{
		"cwd:/a/b/../../etc/passwd",
		"cwd:" + strings.Repeat("/deep", 300),
	} {
		if got := filepath.Dir(a.lockPath(key)); got != a.dir {
			t.Fatalf("lockPath(%q) escaped the locks dir: got dir %q, want %q", key, got, a.dir)
		}
		if ok, err := a.tryLock(key); err != nil || !ok {
			t.Fatalf("a.tryLock(%q) = (%v, %v), want (true, nil)", key, ok, err)
		}
		if ok, err := b.tryLock(key); err != nil || ok {
			t.Fatalf("b.tryLock(%q) must be refused while a holds it; got (%v, %v)", key, ok, err)
		}
	}
}

// TestCrossLockFailsClosedOnUncreatableDir: when the locks directory cannot be
// created (its parent is a regular file), tryLock returns an error so the caller
// fails closed rather than running unprotected.
func TestCrossLockFailsClosedOnUncreatableDir(t *testing.T) {
	base := t.TempDir()
	// A regular file where the state dir is expected: MkdirAll(base/file/locks) fails.
	fileAsStateDir := filepath.Join(base, "file")
	if err := os.WriteFile(fileAsStateDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := newCrossLock(fileAsStateDir)
	if ok, err := c.tryLock("cwd:/w"); err == nil || ok {
		t.Fatalf("tryLock on an uncreatable locks dir = (%v, %v), want (false, error)", ok, err)
	}
}
