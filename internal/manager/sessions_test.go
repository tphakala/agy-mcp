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
