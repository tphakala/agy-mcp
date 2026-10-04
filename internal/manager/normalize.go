package manager

import (
	"os"
	"path/filepath"
	"strings"
)

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
		// consumer decide what to do with it. Every live caller already guarantees a
		// non-empty value (StartJob fails closed, readSessions guards, dirsInclude
		// skips empty entries), so this is defence in depth.
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

// dirsInclude reports whether any of dirs names cwd, an already normalized
// directory. A relative entry is resolved against cwd, the directory agy runs in,
// and each entry gets normalizeCwd's canonical form, so a trailing slash or a
// symlinked alias still counts as a match.
//
// The canonical form alone is not trusted, because it is lexical first:
// filepath.Join cleans "<cwd>/link/.." to cwd before any symlink is resolved,
// while a POSIX kernel resolves ".." from the link's target. So an entry must also be
// the same directory as cwd when the OS resolves it as written. The same check
// lets an entry that differs from cwd only in letter case count as cwd, but only
// where the filesystem itself says they are one directory. A rooted or
// volume-relative entry (see rootRelative) never counts. Whenever the checks
// disagree, or cwd cannot be stat'ed, the entry does not count: agy then gets
// cwd as well as the entry, and a duplicate --add-dir was MEASURED harmless on
// agy 1.2.9 (macOS), while dropping cwd would lose the project's rule files on
// agy that needs --add-dir to load them.
func dirsInclude(dirs []string, cwd string) bool {
	if cwd == "" {
		return false
	}
	var cwdInfo os.FileInfo // stat'ed on the first entry that passes the lexical check
	for _, d := range dirs {
		if d == "" {
			// filepath.Join(cwd, "") is cwd itself, so an empty entry would count as
			// a match and drop the implicit workspace while naming no directory.
			continue
		}
		if rootRelative(d) {
			continue
		}
		joined, asWritten := d, d
		if !filepath.IsAbs(d) {
			// Joined by hand, not with filepath.Join, so ".." is left for the OS.
			asWritten = joinAsWritten(cwd, d)
			joined = filepath.Join(cwd, d)
		}
		n, err := normalizeCwd(joined)
		if err != nil || (n != cwd && !strings.EqualFold(n, cwd)) {
			continue
		}
		if cwdInfo == nil {
			if cwdInfo, err = os.Stat(cwd); err != nil {
				return false
			}
		}
		if info, err := os.Stat(asWritten); err == nil && os.SameFile(info, cwdInfo) {
			return true
		}
	}
	return false
}

// joinAsWritten appends d to cwd with one separator and no cleaning, so ".." is
// left for the OS to resolve. A cwd that already ends in a separator (a
// filesystem root) gets none added, which would give "//dir". cwd must be
// non-empty.
func joinAsWritten(cwd, d string) string {
	if os.IsPathSeparator(cwd[len(cwd)-1]) {
		return cwd + d
	}
	return cwd + string(filepath.Separator) + d
}

// rootRelative reports whether d is neither absolute nor relative to cwd: a
// rooted path without a volume (`\x`) or one with a volume but no root
// (`C:repo`). These are Windows-only shapes; agy would resolve them against a
// drive root or a drive's current directory, which is not measured, so they
// never stand in for cwd (issue #192). On POSIX it is always false. d must be
// non-empty.
func rootRelative(d string) bool {
	return !filepath.IsAbs(d) && (filepath.VolumeName(d) != "" || os.IsPathSeparator(d[0]))
}
