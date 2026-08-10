package manager

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
)

// TestBuildAgyArgs pins the agy command line: the fixed flags (ending in
// --disable-slash-commands so prompts stay literal), then --model, --effort,
// --mode, --agent, --sandbox, repeated --add-dir, --conversation, --json-schema,
// and finally -p with the prompt, with the optional flags omitted when their
// fields are empty (and --sandbox omitted when false).
func TestBuildAgyArgs(t *testing.T) {
	got := buildAgyArgs(StartRequest{
		Prompt:         "review this",
		Model:          "gemini-3.1-pro-high",
		Effort:         "high",
		Mode:           "plan",
		Agent:          "reviewer",
		Sandbox:        true,
		Dirs:           []string{"/a", "/b"},
		ConversationID: "cid-123",
		JSONSchema:     `{"type":"object"}`,
		Timeout:        20 * time.Minute,
	})
	want := []string{
		"--dangerously-skip-permissions",
		"--print-timeout", "20m0s",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"--model", "gemini-3.1-pro-high",
		"--effort", "high",
		"--mode", "plan",
		"--agent", "reviewer",
		"--sandbox",
		"--add-dir", "/a", "--add-dir", "/b",
		"--conversation", "cid-123",
		"--json-schema", `{"type":"object"}`,
		"-p", "review this",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buildAgyArgs full =\n  %q\nwant\n  %q", got, want)
	}

	// Minimal request: no model, dirs, or conversation -> only the fixed flags and
	// the prompt. --output-format is fixed, not optional: the whole job pipeline
	// decodes the stream-json events, so it is never omitted.
	got = buildAgyArgs(StartRequest{Prompt: "hi", Timeout: time.Minute})
	want = []string{
		"--dangerously-skip-permissions",
		"--print-timeout", "1m0s",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"-p", "hi",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buildAgyArgs minimal =\n  %q\nwant\n  %q", got, want)
	}

	// A model value carrying spaces must survive as ONE argument, immediately after
	// --model. A bare display label is still a supported input (StartJob's modelID
	// forwards one unchanged, and agy accepts it when no --effort accompanies it),
	// and every other model fixture in this test is a space-free slug: a builder
	// that truncated the value at whitespace satisfies every other assertion here
	// and fails only this one.
	const spaced = "Gemini 3.1 Pro (High)"
	got = buildAgyArgs(StartRequest{Prompt: "hi", Model: spaced, Timeout: time.Minute})
	if i := slices.Index(got, "--model"); i < 0 || i+1 >= len(got) || got[i+1] != spaced {
		t.Fatalf("buildAgyArgs must pass a spaced model as one argument after --model, got %q", got)
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
