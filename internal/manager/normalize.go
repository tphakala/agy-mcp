package manager

import "path/filepath"

// normalizeCwd canonicalizes a working directory so the agy conversation-cache
// lookups, the spawned cmd.Dir, and the persisted meta all agree on one
// spelling. A trailing slash, a relative path, or a symlinked alias would
// otherwise miss a cache entry, so continue_latest would silently start a new
// conversation and a list_sessions directory filter would match nothing.
//
// The gate key is no longer among the consumers: fresh runs stopped being keyed
// by directory once the conversation id started arriving in agy's own stream, so
// keyFor reads only the conversation id.
//
// EvalSymlinks also aligns the key with the physical path agy records: the
// supervisor sets cmd.Dir to this value, so agy's own getcwd returns the
// symlink-resolved path and keys last_conversations.json by that.
func normalizeCwd(cwd string) (string, error) {
	if cwd == "" {
		// filepath.Abs("") resolves to the process working directory, which would
		// turn "no directory" into a confident claim about an unrelated one: a cache
		// lookup against the manager's own cwd, or a cmd.Dir the caller never asked
		// for. An empty cwd has no canonical form, so keep it empty and let each
		// consumer decide what to do with it. Both live callers already guarantee a
		// non-empty value (StartJob fails closed, readSessions guards), so this is
		// defence in depth.
		return "", nil
	}
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
