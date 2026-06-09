package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Session pairs a workspace path with its most recent agy conversation UUID.
type Session struct {
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversation_id"`
}

// agyCachePath returns the path to last_conversations.json, honoring HOME.
func agyCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json")
}

// ListSessions returns known conversations, optionally filtered to one dir.
func (m *Manager) ListSessions(dir string) ([]Session, error) {
	return readSessions(agyCachePath(), dir)
}

func readSessions(cacheFile, filterDir string) ([]Session, error) {
	b, err := os.ReadFile(cacheFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	var out []Session
	for ws, id := range raw {
		if filterDir != "" && ws != filterDir {
			continue
		}
		out = append(out, Session{Workspace: ws, ConversationID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Workspace < out[j].Workspace })
	return out, nil
}
