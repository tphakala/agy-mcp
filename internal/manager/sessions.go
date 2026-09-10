package manager

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Session pairs a workspace path with its most recent agy conversation UUID.
//
// The jsonschema tags reach clients through list_sessions' output schema, which
// nests this type under "sessions"; see TestToolDefinitionsDescribeThemselves.
type Session struct {
	Workspace      string `json:"workspace" jsonschema:"absolute path of the workspace directory this conversation belongs to; pass it as cwd to continue the thread there"`
	ConversationID string `json:"conversation_id" jsonschema:"most recent conversation id agy recorded for this workspace; pass it as conversation_id to agy_run or agy_run_sync"`
}

// agyCachePath returns the path to last_conversations.json, honoring HOME. An
// empty return (the home dir is unresolvable) disables both features that read the
// cache, continue_latest and session listing, so log it rather than failing
// silently; the cause is almost always a missing HOME in a restricted environment.
func agyCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("cannot resolve home dir for agy conversation cache; continue_latest and session listing disabled: %v", err)
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json")
}

// ListSessions returns known conversations, optionally filtered to one dir. It
// reads m.cacheFile so it shares the manager's single source of truth for the agy
// cache path (and is injectable in tests), as resolveLatest does.
//
// An unfiltered listing omits the ephemeral agy-openai-shim per-call workspaces
// (see isTransientWorkspace); passing dir returns whatever matches it, transient
// or not.
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
		// Drop the ephemeral agy-openai-shim per-call workspaces from an unfiltered
		// listing (issue #167): agy-mcp never starts a run in one and they dominate
		// the shared cache. An explicit dir filter is honored above, so a caller that
		// asks for one specific transient dir still gets it.
		if cleanFilter == "" && isTransientWorkspace(ws) {
			continue
		}
		out = append(out, Session{Workspace: ws, ConversationID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Workspace < out[j].Workspace })
	return out, nil
}

// Transient agy-openai-shim workspaces are laid out as
// <...>/agy-openai-shim/agy-call-<n>. agy-openai-shim is a separate tool, so
// agy-mcp never starts a run in one of these per-call dirs, and on a live cache
// they were over 99% of the entries (issue #167). That noise is why the
// unfiltered listing drops them.
const (
	agyShimDir    = "agy-openai-shim"
	agyCallPrefix = "agy-call-"
)

// isTransientWorkspace reports whether ws is one of the ephemeral per-call
// workspaces agy-openai-shim creates. It requires BOTH signals, the
// agy-openai-shim parent segment AND an agy-call- leaf, so a real workspace that
// merely shares one of those names (for example a checkout of the shim repo
// itself, agy-openai-shim/docs) is never dropped from the listing. ws is cleaned
// first so a trailing separator cannot push Base onto the wrong segment.
func isTransientWorkspace(ws string) bool {
	ws = filepath.Clean(ws)
	return filepath.Base(filepath.Dir(ws)) == agyShimDir &&
		strings.HasPrefix(filepath.Base(ws), agyCallPrefix)
}
