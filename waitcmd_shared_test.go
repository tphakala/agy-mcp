package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// setFakeHome points the home-directory lookup at a fresh temp dir on both Unix
// (HOME) and Windows (USERPROFILE, which os.UserHomeDir reads there), so a test
// that resolves the wait config stays hermetic and never touches the real user's
// agy conversation cache on either platform.
func setFakeHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// writeTerminalJob creates <stateDir>/jobs/<id> holding a done job: meta.json,
// an out file "RESULT", and an exit_code sentinel "0". The args carry
// --output-format stream-json, as every job this build starts does, but there is
// no result.json, so the job reads as done with a partial result. That is all
// these tests need (they assert on the state, not the payload). It writes only
// files, so it and the tests that use it run on Windows too.
func writeTerminalJob(t *testing.T, stateDir, id string) {
	t.Helper()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := jobstore.Meta{
		ID:        id,
		StartedAt: time.Now(),
		Args:      []string{"--output-format", "stream-json"},
	}
	b, err := jsonMarshalForTest(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.MetaPath(dir), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.OutPath(dir), []byte("RESULT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.ExitCodePath(dir), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hookPayload builds the stdin JSON hook-wait expects from a PostToolUse hook,
// with the given state carried in the tool response (the state the tool call
// itself observed, distinct from whatever the job's state is on disk by the time
// the hook runs).
func hookPayload(tool, id, state string) string {
	return fmt.Sprintf(`{"tool_name":%q,"tool_response":{"job_id":%q,"state":%q}}`, tool, id, state)
}
