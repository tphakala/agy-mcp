package jobstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestRejectsUnsafeJobID(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`} {
		if _, err := s.Create(Meta{ID: id}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Create(%q) err = %v, want ErrInvalidID", id, err)
		}
		if _, err := s.Load(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Load(%q) err = %v, want ErrInvalidID", id, err)
		}
		if err := s.Remove(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Remove(%q) err = %v, want ErrInvalidID", id, err)
		}
		if _, ok := s.ExitCode(id); ok {
			t.Errorf("ExitCode(%q) ok = true, want false", id)
		}
	}
}

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
	if got.Cwd != "/work" || got.BootID != "boot-abc" {
		t.Fatalf("round-trip cwd/bootid mismatch: %+v", got)
	}
	if !slices.Equal(got.Args, m.Args) {
		t.Fatalf("round-trip args = %v, want %v", got.Args, m.Args)
	}
	if !got.StartedAt.Equal(m.StartedAt) {
		t.Fatalf("round-trip startedAt = %v, want %v", got.StartedAt, m.StartedAt)
	}
}

func TestUpdateMetaRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMeta(Meta{ID: "j", PID: 99, AgyPath: "/x"}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	got, err := s.Load("j")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 99 || got.AgyPath != "/x" {
		t.Fatalf("UpdateMeta round-trip = %+v", got)
	}
}

func TestSetConversationID(t *testing.T) {
	const first = "conv-1"
	s := New(t.TempDir())
	if _, err := s.Create(Meta{ID: "j", PID: 4242, AgyPath: "/usr/bin/agy"}); err != nil {
		t.Fatal(err)
	}
	// Set on an unset job: persists the id and returns it, preserving other fields.
	got, err := s.SetConversationID("j", first)
	if err != nil {
		t.Fatalf("SetConversationID: %v", err)
	}
	if got != first {
		t.Fatalf("returned id = %q, want %q", got, first)
	}
	reloaded, err := s.Load("j")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ConversationID != first {
		t.Fatalf("persisted id = %q, want %q", reloaded.ConversationID, first)
	}
	if reloaded.PID != 4242 || reloaded.AgyPath != "/usr/bin/agy" {
		t.Fatalf("other fields clobbered: %+v", reloaded)
	}
	// Setting again is a no-op: returns the existing id, does not overwrite.
	got, err = s.SetConversationID("j", "conv-2")
	if err != nil {
		t.Fatalf("second SetConversationID: %v", err)
	}
	if got != first {
		t.Fatalf("second set returned %q, want existing %q", got, first)
	}
	reloaded, err = s.Load("j")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ConversationID != first {
		t.Fatalf("existing id was overwritten: %q", reloaded.ConversationID)
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

func TestListAndRemove(t *testing.T) {
	s := New(t.TempDir())
	_, _ = s.Create(Meta{ID: "a", StartedAt: time.Unix(0, 0)})
	_, _ = s.Create(Meta{ID: "b", StartedAt: time.Now()})
	ids, err := s.List()
	if err != nil || len(ids) != 2 {
		t.Fatalf("List = %v, %v", ids, err)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ids, _ = s.List()
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("after Remove, List = %v", ids)
	}
}
