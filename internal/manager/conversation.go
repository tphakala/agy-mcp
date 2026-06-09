package manager

import (
	"encoding/json"
	"os"
)

func loadCache(cacheFile string) map[string]string {
	b, err := os.ReadFile(cacheFile)
	if err != nil {
		return map[string]string{}
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return map[string]string{}
	}
	return raw
}

// resolveLatest returns the most recent conversation UUID for cwd, if any.
func resolveLatest(cacheFile, cwd string) (string, bool) {
	id, ok := loadCache(cacheFile)[cwd]
	return id, ok && id != ""
}

// snapshotCwd records the cwd's UUID before a run.
func snapshotCwd(cacheFile, cwd string) string {
	return loadCache(cacheFile)[cwd]
}

// captureNewUUID returns the cwd's UUID after a run, but only if it changed from
// the pre-run snapshot. A failed run leaves the cache untouched, so this avoids
// misattributing the failure to a previous conversation.
func captureNewUUID(cacheFile, cwd, before string) (string, bool) {
	after := loadCache(cacheFile)[cwd]
	if after != "" && after != before {
		return after, true
	}
	return "", false
}
