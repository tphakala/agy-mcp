package manager

import (
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

func TestGarbageCollectRemovesExpired(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	_, _ = m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)})
	_, _ = m.store.Create(jobstore.Meta{ID: "fresh", StartedAt: time.Now()})
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("removed = %v, want [old]", removed)
	}
}

func TestGarbageCollectDisabledWhenTTLZero(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4}) // JobTTL 0
	_, _ = m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-100 * time.Hour)})
	removed, _ := m.GarbageCollect()
	if len(removed) != 0 {
		t.Fatalf("TTL 0 should disable GC, removed %v", removed)
	}
}
