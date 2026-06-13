package manager

import "path/filepath"

// normalizeCwd canonicalizes a working directory so the gate key, the agy
// conversation-cache lookups, the spawned cmd.Dir, and the persisted meta all
// agree on one spelling. A trailing slash, a relative path, or a symlinked alias
// would otherwise yield a distinct gate key (two "same dir" fresh runs would not
// serialize, re-exposing the agy session-lock hang the gate exists to prevent)
// and a missed cache entry (continue_latest silently starts a new conversation,
// and a fresh run's id capture never matches).
//
// EvalSymlinks also aligns the key with the physical path agy records: the
// supervisor sets cmd.Dir to this value, so agy's own getcwd returns the
// symlink-resolved path and keys last_conversations.json by that.
func normalizeCwd(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	// Best-effort symlink resolution: a path that does not exist yet (agy will
	// fail on it regardless) or is otherwise unresolvable keeps the cleaned
	// absolute form instead of failing the run here.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}
