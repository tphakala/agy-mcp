package manager

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
)

// TestBuildAgyArgs pins the agy command line: the fixed flags, then --model,
// repeated --add-dir, --conversation, and finally -p with the prompt, with the
// optional flags omitted when their fields are empty.
func TestBuildAgyArgs(t *testing.T) {
	got := buildAgyArgs(StartRequest{
		Prompt:         "review this",
		Model:          "Gemini 3.1 Pro (High)",
		Dirs:           []string{"/a", "/b"},
		ConversationID: "cid-123",
		Timeout:        20 * time.Minute,
	})
	want := []string{
		"--dangerously-skip-permissions",
		"--print-timeout", "20m0s",
		"--model", "Gemini 3.1 Pro (High)",
		"--add-dir", "/a", "--add-dir", "/b",
		"--conversation", "cid-123",
		"-p", "review this",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buildAgyArgs full =\n  %q\nwant\n  %q", got, want)
	}

	// Minimal request: no model, dirs, or conversation -> only timeout and prompt.
	got = buildAgyArgs(StartRequest{Prompt: "hi", Timeout: time.Minute})
	want = []string{"--dangerously-skip-permissions", "--print-timeout", "1m0s", "-p", "hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildAgyArgs minimal =\n  %q\nwant\n  %q", got, want)
	}
}

// TestStartJobRejectsConversationIDWithContinueLatest: supplying both an
// explicit conversation_id and continue_latest is ambiguous (continue_latest
// resolves to an id that would silently overwrite the explicit one, and the
// precedence flips depending on whether the cache resolves). StartJob must
// reject the combination outright rather than pick a confusing winner. The
// check runs before the platform gate, so it is exercised on every OS.
func TestStartJobRejectsConversationIDWithContinueLatest(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.StartJob(StartRequest{
		Prompt:         "hi",
		Cwd:            t.TempDir(),
		ConversationID: "abc",
		ContinueLatest: true,
	})
	if err == nil {
		t.Fatal("StartJob must reject conversation_id + continue_latest together")
	}
	if !strings.Contains(err.Error(), "continue_latest") || !strings.Contains(err.Error(), "conversation_id") {
		t.Fatalf("error = %v, want it to name both conflicting fields", err)
	}
}
