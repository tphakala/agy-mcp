package manager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListSessionsFromCache(t *testing.T) {
	cache := t.TempDir()
	data := `{"/home/u/proj":"uuid-1","/home/u/other":"uuid-2"}`
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(filepath.Join(cache, "last_conversations.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
}

func TestListSessionsFilteredByDir(t *testing.T) {
	cache := t.TempDir()
	data := `{"/home/u/proj":"uuid-1","/home/u/other":"uuid-2"}`
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(filepath.Join(cache, "last_conversations.json"), "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConversationID != "uuid-1" {
		t.Fatalf("got %+v", sessions)
	}
}

// TestListSessionsFilterCanonicalizesSymlink verifies the session filter matches
// the resolved path agy keys its cache by, even when the caller passes a
// symlinked alias of that directory. agy keys last_conversations.json by the
// symlink-resolved physical path (its cmd.Dir getcwd), so a Clean-only filter on
// a symlinked alias would never match. The filter must canonicalize the same way
// StartJob canonicalizes a run's cwd (issue #24).
func TestListSessionsFilterCanonicalizesSymlink(t *testing.T) {
	realDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	cache := filepath.Join(t.TempDir(), "last_conversations.json")
	// agy stores the entry under the resolved physical path.
	data := `{"` + resolved + `":"uuid-1"}`
	if err := os.WriteFile(cache, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	// Filtering by the symlinked alias must still find the resolved entry.
	sessions, err := readSessions(cache, link)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConversationID != "uuid-1" {
		t.Fatalf("symlinked filter did not match resolved cache key: got %+v", sessions)
	}
}
