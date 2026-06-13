package manager

import (
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// TestListModelsIncludesStderrOnError: when `agy models` fails, the error must
// carry agy's stderr (e.g. an auth prompt) rather than a bare "exit status 1".
func TestListModelsIncludesStderrOnError(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: "agy: not logged in", Exit: 1})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.ListModels(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want it to include agy's stderr", err)
	}
}

func TestParseModels(t *testing.T) {
	raw := "Gemini 3.5 Flash (Medium)\nGemini 3.1 Pro (High)\n\nClaude Opus 4.6 (Thinking)\n"
	got := parseModels(raw)
	want := []string{"Gemini 3.5 Flash (Medium)", "Gemini 3.1 Pro (High)", "Claude Opus 4.6 (Thinking)"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
