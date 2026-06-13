package manager

import (
	"log"
	"os"
	"path/filepath"
	"sort"
)

// Session pairs a workspace path with its most recent agy conversation UUID.
type Session struct {
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversation_id"`
}

// agyCachePath returns the path to last_conversations.json, honoring HOME. An
// empty return (the home dir is unresolvable) degrades every consumer to "no
// sessions" and disables conversation-id capture, so log it rather than failing
// silently; the cause is almost always a missing HOME in a restricted environment.
func agyCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("cannot resolve home dir for agy conversation cache; session listing and conversation-id capture disabled: %v", err)
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json")
}

// ListSessions returns known conversations, optionally filtered to one dir. It
// reads m.cacheFile so it shares the manager's single source of truth for the agy
// cache path (and is injectable in tests) like the capture/resolve paths.
func (m *Manager) ListSessions(dir string) ([]Session, error) {
	return readSessions(m.cacheFile, dir)
}

func readSessions(cacheFile, filterDir string) ([]Session, error) {
	// loadCache is the single reader for last_conversations.json: it treats a
	// missing file as an empty cache and reports a torn or corrupt read as an
	// error, so this no longer duplicates the read+unmarshal (issue #36).
	raw, err := loadCache(cacheFile)
	if err != nil {
		return nil, err
	}
	cleanFilter := ""
	if filterDir != "" {
		// Canonicalize the filter the same way StartJob canonicalizes a run's cwd
		// (filepath.Abs + best-effort EvalSymlinks; see normalizeCwd), so the filter
		// matches the resolved paths agy keys its cache by. A symlinked or relative
		// filter would otherwise never match a stored entry. normalizeCwd only errors
		// when Abs fails (a relative path with no working directory); fall back to
		// Clean so a filter is still applied.
		if norm, nerr := normalizeCwd(filterDir); nerr == nil {
			cleanFilter = norm
		} else {
			cleanFilter = filepath.Clean(filterDir)
		}
	}
	var out []Session
	for ws, id := range raw {
		if cleanFilter != "" && filepath.Clean(ws) != cleanFilter {
			continue
		}
		out = append(out, Session{Workspace: ws, ConversationID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Workspace < out[j].Workspace })
	return out, nil
}
