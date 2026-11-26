package manager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// validateStartRequest refuses the input combinations that cannot work, before
// any platform or filesystem work, so the caller gets a deterministic error
// whatever the platform.
//
// conversation_id and continue_latest are mutually exclusive: continue_latest
// resolves to a conversation id that would otherwise silently overwrite the
// explicit one, and the precedence even flips depending on whether the cache
// resolves.
//
// An isolated run uses a directory agy-mcp creates and has no workspace, so it
// is refused together with every input that names a directory (cwd, dirs),
// resolves one (continue_latest keys on cwd), or toggles the implicit workspace
// (SkipProjectRules, which the tool layer sets from an explicit project_rules
// false).
func validateStartRequest(req StartRequest) error {
	if req.ContinueLatest && req.ConversationID != "" {
		return errors.New("conversation_id and continue_latest are mutually exclusive: pass one or the other")
	}
	if !req.Isolated {
		return nil
	}
	switch {
	case req.Cwd != "":
		return errors.New("cwd cannot be combined with isolated: an isolated run uses a fresh empty directory agy-mcp creates; name the repository's files by absolute path in the prompt")
	case len(req.Dirs) > 0:
		return errors.New("dirs cannot be combined with isolated: an isolated run has no workspace; name the files by absolute path in the prompt")
	case req.ContinueLatest:
		return errors.New("continue_latest cannot be combined with isolated: a fresh directory has no latest conversation; pass conversation_id")
	case req.SkipProjectRules:
		return errors.New("project_rules cannot be combined with isolated: an isolated run has no workspace, so there are no project rules to load or skip")
	}
	return nil
}

// runDir returns the directory agy runs in. A normal request runs in its own
// normalized cwd. An isolated run gets the work directory inside its job dir,
// recorded in meta.Cwd only: the request keeps Cwd empty so buildAgyArgs, which
// derives --add-dir from the request, gives agy no workspace, and the request
// key and idempotency comparison keep seeing the request the caller made.
func (m *Manager) runDir(req StartRequest, id string) (string, error) {
	if !req.Isolated {
		return req.Cwd, nil
	}
	dir, err := m.store.Dir(id)
	if err != nil {
		return "", fmt.Errorf("locate job directory: %w", err)
	}
	return jobstore.WorkDirPath(dir), nil
}

// prepareRunDir creates the directory runDir chose, for an isolated request, and
// is a no-op otherwise.
func prepareRunDir(req StartRequest, dir string) error {
	if !req.Isolated {
		return nil
	}
	return prepareWorkDir(dir)
}

// prepareWorkDir creates the empty work directory of an isolated run and
// refuses it when a git work tree sits above it. os.Mkdir fails if the path
// exists, so a run never inherits another run's files.
//
// MEASURED against agy 1.5.18 on macOS, one run per shape: an empty directory
// two levels below a git root loaded that repository's AGENTS.md and ran its
// .agents/hooks.json hooks, so the work directory must have no git ancestor. An
// empty non-git directory whose parent held an AGENTS.md did not load it (only
// AGENTS.md was tried there, not hooks). Setting GIT_DIR and GIT_WORK_TREE for a
// non-git directory did not make agy load the repository's rules or hooks, which
// is why only .git entries are looked for. Both the lexical path and its
// symlink-resolved form are walked, since the state directory may reach the work
// directory through a link.
func prepareWorkDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create isolated work directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve isolated work directory: %w", err)
	}
	starts := []string{filepath.Dir(dir)}
	if r := filepath.Dir(resolved); r != starts[0] {
		starts = append(starts, r)
	}
	for _, start := range starts {
		root, found, err := gitAncestor(start)
		if err != nil {
			return fmt.Errorf("check isolated work directory for a git work tree: %w", err)
		}
		if found {
			return fmt.Errorf("isolated runs are refused because the job directory %s is inside the git work tree %s, where agy would load that repository's rule files and hooks; set AGY_MCP_STATE_DIR to a directory outside any git work tree", dir, root)
		}
	}
	return nil
}

// gitAncestor reports the nearest directory at or above dir that holds a .git
// entry. A .git file counts as well as a directory, because worktrees and
// submodules use a file. An error other than "does not exist" is returned
// rather than read as "no repository", so an unreadable ancestor fails closed.
// A directory's .git is checked before the root test, so the filesystem root
// itself is covered.
func gitAncestor(dir string) (root string, found bool, err error) {
	for p := dir; ; {
		_, statErr := os.Lstat(filepath.Join(p, ".git"))
		switch {
		case statErr == nil:
			return p, true, nil
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", false, statErr
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", false, nil
		}
		p = parent
	}
}
