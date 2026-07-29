package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
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

func TestNewUsesConfiguredCacheFile(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 1,
		ConversationCacheFile: "/tmp/agy-test-cache.json"})
	if m.cacheFile != "/tmp/agy-test-cache.json" {
		t.Fatalf("cacheFile = %q, want the configured override", m.cacheFile)
	}
}
