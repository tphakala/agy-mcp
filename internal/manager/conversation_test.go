package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/internal/config"
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
	before, ok := snapshotCwd(cache, "/w") // "old"
	if !ok {
		t.Fatal("snapshot should be readable")
	}
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
	before, ok := snapshotCwd(cache, "/w")
	if !ok {
		t.Fatal("snapshot should be readable")
	}
	// agy failed -> cache unchanged.
	got, changed := captureNewUUID(cache, "/w", before)
	if changed || got != "" {
		t.Fatalf("should not capture stale id: %q,%v", got, changed)
	}
}

func TestLoadCacheMissingFileIsEmptyNotError(t *testing.T) {
	cache, err := loadCache(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing cache is a normal empty cache, got error: %v", err)
	}
	if len(cache) != 0 {
		t.Fatalf("want empty cache, got %v", cache)
	}
}

func TestLoadCacheTornReadIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCache(p); err == nil {
		t.Fatal("a torn/unparsable cache read must surface an error")
	}
}

func TestResolveLatestNotFoundOnTornCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveLatest(p, "/w"); ok {
		t.Fatal("a torn cache must resolve to not-found, not to a bogus id")
	}
}

func TestSnapshotCwdUnknownOnTornCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last_conversations.json")
	if err := os.WriteFile(p, []byte(`{"torn`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshotCwd(p, "/w"); ok {
		t.Fatal("a torn cache must report the snapshot as unknown")
	}
}

func TestSnapshotCwdOkOnMissingCache(t *testing.T) {
	before, ok := snapshotCwd(filepath.Join(t.TempDir(), "absent.json"), "/w")
	if !ok || before != "" {
		t.Fatalf("missing cache = valid empty snapshot, got %q,%v", before, ok)
	}
}

func TestNewUsesConfiguredCacheFile(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 1,
		ConversationCacheFile: "/tmp/agy-test-cache.json"})
	if m.cacheFile != "/tmp/agy-test-cache.json" {
		t.Fatalf("cacheFile = %q, want the configured override", m.cacheFile)
	}
}
