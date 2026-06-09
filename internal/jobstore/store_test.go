package jobstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAndLoadMeta(t *testing.T) {
	s := New(t.TempDir())
	m := Meta{
		ID:        "job123",
		AgyPath:   "/usr/bin/agy",
		Args:      []string{"-p", "hi"},
		Cwd:       "/work",
		StartedAt: time.Unix(1000, 0).UTC(),
		PID:       4242,
		BootID:    "boot-abc",
	}
	dir, err := s.Create(m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
	got, err := s.Load("job123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PID != 4242 || got.AgyPath != "/usr/bin/agy" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestExitCodeSentinel(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ExitCode("j"); ok {
		t.Fatal("exit code should be absent before completion")
	}
	if err := s.WriteExitCode("j", 0); err != nil {
		t.Fatal(err)
	}
	code, ok := s.ExitCode("j")
	if !ok || code != 0 {
		t.Fatalf("ExitCode = %d,%v", code, ok)
	}
}

func TestListAndGC(t *testing.T) {
	s := New(t.TempDir())
	_, _ = s.Create(Meta{ID: "old", StartedAt: time.Unix(0, 0)})
	_, _ = s.Create(Meta{ID: "new", StartedAt: time.Now()})
	ids, err := s.List()
	if err != nil || len(ids) != 2 {
		t.Fatalf("List = %v, %v", ids, err)
	}
	removed, err := s.GC(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("GC removed = %v", removed)
	}
}
