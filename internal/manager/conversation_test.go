package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeCache(t *testing.T, dir string, kv map[string]string) string {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveLatestForCwd(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, map[string]string{"/w": "uuid-existing"})
	id, ok := resolveLatest(cache, "/w")
	if !ok || id != "uuid-existing" {
		t.Fatalf("resolveLatest = %q,%v", id, ok)
	}
	if _, ok := resolveLatest(cache, "/missing"); ok {
		t.Fatal("missing cwd should not resolve")
	}
}

func TestCaptureNewUUIDByDiff(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, map[string]string{"/w": "old"})
	before := snapshotCwd(cache, "/w") // "old"
	// Simulate agy creating a new conversation for /w.
	_ = writeCache(t, dir, map[string]string{"/w": "new"})
	got, changed := captureNewUUID(cache, "/w", before)
	if !changed || got != "new" {
		t.Fatalf("capture = %q,%v", got, changed)
	}
}

func TestCaptureNoChangeOnFailedRun(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, map[string]string{"/w": "old"})
	before := snapshotCwd(cache, "/w")
	// agy failed -> cache unchanged.
	got, changed := captureNewUUID(cache, "/w", before)
	if changed || got != "" {
		t.Fatalf("should not capture stale id: %q,%v", got, changed)
	}
}
