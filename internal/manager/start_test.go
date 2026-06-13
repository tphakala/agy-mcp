package manager

import (
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/internal/config"
)

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
