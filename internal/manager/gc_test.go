package manager

import (
	"context"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

func TestGarbageCollectRemovesExpired(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.Create(jobstore.Meta{ID: "fresh", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("removed = %v, want [old]", removed)
	}
}

func TestGCInterval(t *testing.T) {
	cases := []struct{ ttl, want time.Duration }{
		{0, 0},                          // disabled
		{-time.Second, 0},               // disabled
		{2 * time.Hour, time.Hour},      // ttl/2
		{4 * time.Minute, 2 * time.Minute}, // ttl/2, above the floor
		{time.Second, time.Minute},      // ttl/2 below the floor -> floored to 1m
	}
	for _, c := range cases {
		if got := gcInterval(c.ttl); got != c.want {
			t.Errorf("gcInterval(%v) = %v, want %v", c.ttl, got, c.want)
		}
	}
}

func TestRunPeriodicGCCollectsAndStops(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, JobTTL: time.Hour})
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.runPeriodicGC(ctx, 10*time.Millisecond); close(done) }()

	// The ticker collects the expired job.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ids, _ := m.store.List()
		if len(ids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic GC did not collect the expired job")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cancelling the context stops the loop and returns.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodicGC did not return after context cancellation")
	}
}

func TestGarbageCollectDisabledWhenTTLZero(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4}) // JobTTL 0
	if _, err := m.store.Create(jobstore.Meta{ID: "old", StartedAt: time.Now().Add(-100 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	removed, err := m.GarbageCollect()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("TTL 0 should disable GC, removed %v", removed)
	}
}
