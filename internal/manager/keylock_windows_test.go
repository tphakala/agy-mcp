package manager

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFlockExclusiveNBContention: a second handle to the same file cannot take the
// exclusive lock while the first holds it, and can once the first handle closes
// (the release-on-close behavior crossLock relies on).
func TestFlockExclusiveNBContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.lock")
	f1, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Guard against a fatal between here and the explicit Close below: an open
	// handle would block t.TempDir's RemoveAll on Windows. A double close is benign.
	t.Cleanup(func() { _ = f1.Close() })
	ok, err := flockExclusiveNB(f1.Fd())
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v, want true,nil", ok, err)
	}

	f2, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()
	ok, err = flockExclusiveNB(f2.Fd())
	if err != nil || ok {
		t.Fatalf("contended lock: ok=%v err=%v, want false,nil", ok, err)
	}

	// Closing the first handle releases the lock, so the second can now take it.
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}
	ok, err = flockExclusiveNB(f2.Fd())
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v, want true,nil", ok, err)
	}
}

// TestCrossLockSerializesAcrossInstances: two crossLock instances sharing a state
// dir (standing in for two agy-mcp processes) cannot both hold the same key, and
// the key frees once the holder unlocks.
func TestCrossLockSerializesAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	a := newCrossLock(dir)
	b := newCrossLock(dir)
	const key = "conv-123"

	got, err := a.tryLock(key)
	if err != nil || !got {
		t.Fatalf("a.tryLock: got=%v err=%v, want true,nil", got, err)
	}
	got, err = b.tryLock(key)
	if err != nil || got {
		t.Fatalf("b.tryLock while a holds it: got=%v err=%v, want false,nil", got, err)
	}
	a.unlock(key)
	got, err = b.tryLock(key)
	if err != nil || !got {
		t.Fatalf("b.tryLock after a released: got=%v err=%v, want true,nil", got, err)
	}
	b.unlock(key)
}
