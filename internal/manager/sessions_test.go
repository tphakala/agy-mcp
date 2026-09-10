package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// wantUUID1 is the conversation id the session-filter tests expect to survive
// their directory filter.
const wantUUID1 = "uuid-1"

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

// A missing cache file is a normal empty cache (fresh agy install, no
// conversations yet), so readSessions must return no sessions and no error. This
// locks in the behavior now that readSessions delegates the read to loadCache.
func TestListSessionsMissingCacheIsEmpty(t *testing.T) {
	sessions, err := readSessions(filepath.Join(t.TempDir(), "does-not-exist.json"), "")
	if err != nil {
		t.Fatalf("a missing cache must not error: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("a missing cache must yield no sessions, got %d", len(sessions))
	}
}

// A malformed cache (a torn or corrupt write) must surface as an error, not be
// mistaken for an empty cache: agy rewrites last_conversations.json in place, so a
// concurrent read can be torn. This covers the error side of the loadCache
// contract that readSessions now relies on.
func TestListSessionsMalformedCacheErrors(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "last_conversations.json")
	if err := os.WriteFile(cache, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessions(cache, ""); err == nil {
		t.Fatal("a malformed cache must return an error, not an empty session list")
	}
}

func TestListSessionsFilteredByDir(t *testing.T) {
	skipIfWindows(t, "Unix-specific: hardcoded /home/u paths do not survive Windows path canonicalization; cross-platform filtering is covered by TestListSessionsFilterMatchesRealDir")
	cache := t.TempDir()
	data := `{"/home/u/proj":"uuid-1","/home/u/other":"uuid-2"}`
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(filepath.Join(cache, "last_conversations.json"), "/home/u/proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConversationID != wantUUID1 {
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
	skipIfWindows(t, "Unix-specific: uses os.Symlink (privileged on Windows) and a raw path in JSON")
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
	if len(sessions) != 1 || sessions[0].ConversationID != wantUUID1 {
		t.Fatalf("symlinked filter did not match resolved cache key: got %+v", sessions)
	}
}

// TestListSessionsFilterMatchesRealDir exercises the directory filter with a real,
// platform-native path key, so the filter path is covered on every OS (the
// hardcoded-Unix-path filter tests are skipped off Linux). Using json.Marshal for
// the cache keeps a Windows path (with backslashes) valid in the JSON.
func TestListSessionsFilterMatchesRealDir(t *testing.T) {
	dir := t.TempDir()
	norm, err := normalizeCwd(dir)
	if err != nil {
		t.Fatalf("normalizeCwd: %v", err)
	}
	other := filepath.Join(dir, "other") // a distinct path that must be filtered out
	cache := filepath.Join(t.TempDir(), "last_conversations.json")
	data, err := json.Marshal(map[string]string{norm: wantUUID1, other: "uuid-2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(cache, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConversationID != wantUUID1 {
		t.Fatalf("directory filter did not match the real-dir cache key: got %+v", sessions)
	}
}

// TestListSessionsFiltersTransientShimWorkspaces: an unfiltered listing must drop
// the ephemeral agy-openai-shim/agy-call-<n> workspaces (issue #167) and keep the
// real ones. Sabotage: delete the isTransientWorkspace skip in readSessions and
// the two transient entries reappear (got 4, want 2).
func TestListSessionsFiltersTransientShimWorkspaces(t *testing.T) {
	root := t.TempDir()
	real1 := filepath.Join(root, "proj")
	real2 := filepath.Join(root, "other")
	shim := filepath.Join(root, "agy-openai-shim")
	cache := filepath.Join(t.TempDir(), "last_conversations.json")
	data, err := json.Marshal(map[string]string{
		real1:                             "uuid-1",
		real2:                             "uuid-2",
		filepath.Join(shim, "agy-call-1"): "uuid-shim-1",
		filepath.Join(shim, "agy-call-2"): "uuid-shim-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(cache, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 real ones (transient shim entries must be dropped): %+v", len(sessions), sessions)
	}
	for _, s := range sessions {
		if filepath.Base(filepath.Dir(s.Workspace)) == "agy-openai-shim" {
			t.Errorf("transient shim workspace leaked into the unfiltered listing: %q", s.Workspace)
		}
	}
}

// TestListSessionsExplicitDirReturnsTransientWorkspace: an explicit dir filter is
// honored verbatim, even when it points at a transient shim workspace, so the
// exclusion is scoped to the unfiltered listing. Sabotage: drop the
// cleanFilter=="" guard on the skip (filter transient unconditionally) and this
// returns nothing.
func TestListSessionsExplicitDirReturnsTransientWorkspace(t *testing.T) {
	root := t.TempDir()
	shimCall := filepath.Join(root, "agy-openai-shim", "agy-call-7")
	// The filter canonicalizes with normalizeCwd, so store the entry under the same
	// canonical spelling the filter resolves to (the dir does not exist, so this is
	// the cleaned absolute form).
	norm, err := normalizeCwd(shimCall)
	if err != nil {
		t.Fatalf("normalizeCwd: %v", err)
	}
	cache := filepath.Join(t.TempDir(), "last_conversations.json")
	data, err := json.Marshal(map[string]string{norm: wantUUID1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := readSessions(cache, shimCall)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConversationID != wantUUID1 {
		t.Fatalf("an explicit dir filter for a transient workspace must return it, got %+v", sessions)
	}
}

// TestIsTransientWorkspace pins the detection boundary: both the agy-openai-shim
// parent segment AND an agy-call- leaf are required, and only the immediate parent
// counts, so a real workspace is never dropped and a deeper layout degrades to
// "not transient" rather than over-matching.
func TestIsTransientWorkspace(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "agy-openai-shim")
	cases := []struct {
		name string
		ws   string
		want bool
	}{
		{"shim call dir", filepath.Join(shim, "agy-call-42"), true},
		{"shim call dir empty suffix", filepath.Join(shim, "agy-call-"), true},
		{"shim call dir trailing separator", filepath.Join(shim, "agy-call-9") + string(filepath.Separator), true},
		{"real project", filepath.Join(root, "proj"), false},
		{"agy-call leaf not under shim", filepath.Join(root, "agy-call-9"), false},
		{"non-call leaf under shim", filepath.Join(shim, "docs"), false},
		{"call dir nested deeper under shim", filepath.Join(shim, "sub", "agy-call-1"), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientWorkspace(tc.ws); got != tc.want {
				t.Errorf("isTransientWorkspace(%q) = %v, want %v", tc.ws, got, tc.want)
			}
		})
	}
}
